package hostsecurity

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func packageSources(packages []Package) map[string][]Package {
	out := map[string][]Package{}
	for _, p := range packages {
		out[p.Source] = append(out[p.Source], p)
	}
	return out
}

func (i *Intelligence) debian(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, error) {
	raw, source, err := i.fetch(ctx, "debian", "https://security-tracker.debian.org/tracker/data/json")
	if err != nil {
		return nil, []Source{source}, err
	}
	release := h.Codename
	if release == "" {
		release = map[string]string{"10": "buster", "11": "bullseye", "12": "bookworm", "13": "trixie", "14": "forky"}[h.Version]
	}
	if release == "" {
		return nil, []Source{source}, errors.New("Debian release is unknown")
	}
	bySource := packageSources(packages)
	out := []Finding{}
	releaseSeen := false
	err = decodeObject(raw, false, func(name string, record json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		targets := bySource[name]
		if len(targets) == 0 {
			return nil
		}
		var advisories map[string]struct {
			Description string `json:"description"`
			Releases    map[string]struct {
				Status  string `json:"status"`
				Fixed   string `json:"fixed_version"`
				Urgency string `json:"urgency"`
			} `json:"releases"`
		}
		if err := json.Unmarshal(record, &advisories); err != nil {
			return err
		}
		for cve, adv := range advisories {
			status, ok := adv.Releases[release]
			if !ok {
				continue
			}
			releaseSeen = true
			if len(CVEs(cve)) != 1 {
				continue
			}
			if status.Fixed == "0" {
				continue
			}
			for _, p := range targets {
				state := "confirmed"
				reason := "Debian tracker marks this source package open for " + release
				if status.Fixed != "" {
					comparison, e := CompareVersions(ctx, s.Runner, "apt", p.SourceVersion, status.Fixed)
					if e != nil {
						return e
					}
					if comparison >= 0 {
						continue
					}
					reason = "installed Debian source version is below the release-specific fixed version"
				} else if status.Status != "open" {
					state = "unverified"
					reason = "Debian tracker cannot establish an affected version"
				}
				severity := strings.TrimSuffix(status.Urgency, "**")
				if severity == "unimportant" {
					severity = "low"
				}
				out = append(out, finding(h, p, cve, cve, status.Fixed, severity, state, reason, source))
			}
		}
		return nil
	})
	if err == nil && !releaseSeen {
		err = errors.New("no Debian release-specific package coverage found")
	}
	return out, []Source{source}, err
}

func (i *Intelligence) alpine(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, error) {
	parts := strings.Split(h.Version, ".")
	if len(parts) < 2 {
		return nil, nil, errors.New("Alpine release is unknown")
	}
	release := "v" + strings.Join(parts[:2], ".")
	out := []Finding{}
	sources := []Source{}
	var failures []error
	bySource := packageSources(packages)
	for _, repo := range []string{"main", "community"} {
		raw, source, err := i.fetch(ctx, "alpine-"+repo, "https://secdb.alpinelinux.org/"+release+"/"+repo+".json")
		sources = append(sources, source)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		var data struct {
			Release  string `json:"distroversion"`
			Packages []struct {
				Pkg struct {
					Name  string              `json:"name"`
					Fixes map[string][]string `json:"secfixes"`
				} `json:"pkg"`
			} `json:"packages"`
		}
		if err = json.Unmarshal(raw, &data); err != nil {
			failures = append(failures, err)
			continue
		}
		if data.Release != release {
			failures = append(failures, errors.New("Alpine advisory release mismatch"))
			continue
		}
		for _, entry := range data.Packages {
			for _, p := range bySource[entry.Pkg.Name] {
				for fixed, ids := range entry.Pkg.Fixes {
					comparison, e := CompareVersions(ctx, s.Runner, "apk", p.Version, fixed)
					if e != nil {
						return out, sources, e
					}
					if comparison >= 0 {
						continue
					}
					for _, id := range CVEs(strings.Join(ids, " ")) {
						out = append(out, finding(h, p, id, id, fixed, "unknown", "confirmed", "Alpine secdb lists a fixed release newer than the installed package", source))
					}
				}
			}
		}
	}
	return out, sources, errors.Join(failures...)
}

func (i *Intelligence) arch(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, error) {
	raw, source, err := i.fetch(ctx, "arch", "https://security.archlinux.org/all.json")
	if err != nil {
		return nil, []Source{source}, err
	}
	var records []struct {
		Name     string   `json:"name"`
		Packages []string `json:"packages"`
		Status   string   `json:"status"`
		Severity string   `json:"severity"`
		Affected string   `json:"affected"`
		Fixed    string   `json:"fixed"`
		Issues   []string `json:"issues"`
	}
	if err = json.Unmarshal(raw, &records); err != nil {
		return nil, []Source{source}, err
	}
	byName := map[string][]Package{}
	for _, p := range packages {
		byName[p.Name] = append(byName[p.Name], p)
	}
	out := []Finding{}
	for _, record := range records {
		for _, name := range record.Packages {
			for _, p := range byName[name] {
				state := "confirmed"
				reason := "Arch tracker identifies an affected package release"
				if record.Fixed != "" {
					c, e := CompareVersions(ctx, s.Runner, "pacman", p.Version, record.Fixed)
					if e != nil {
						return out, []Source{source}, e
					}
					if c >= 0 {
						continue
					}
				}
				if record.Status != "Vulnerable" && record.Status != "Fixed" {
					state = "unverified"
					reason = "Arch tracker status is unknown"
				}
				if record.Affected == "" && record.Fixed == "" {
					state = "unverified"
					reason = "Arch tracker has no affected version evidence"
				}
				for _, id := range CVEs(strings.Join(record.Issues, " ")) {
					out = append(out, finding(h, p, id, record.Name, record.Fixed, record.Severity, state, reason, source))
				}
			}
		}
	}
	return out, []Source{source}, nil
}

func (i *Intelligence) ubuntu(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, error) {
	raw, source, err := i.fetch(ctx, "ubuntu", "https://usn.ubuntu.com/usn-db/database.json.bz2")
	if err != nil {
		return nil, []Source{source}, err
	}
	if h.Codename == "" {
		return nil, []Source{source}, errors.New("Ubuntu release codename is missing")
	}
	byName := map[string][]Package{}
	for _, p := range packages {
		byName[p.Name] = append(byName[p.Name], p)
	}
	out := []Finding{}
	releaseSeen := false
	err = decodeObject(raw, bytes.HasPrefix(raw, []byte("BZh")), func(id string, record json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var notice struct {
			CVEs     []string `json:"cves"`
			Releases map[string]struct {
				Binaries map[string]struct {
					Version string `json:"version"`
				} `json:"binaries"`
			} `json:"releases"`
		}
		if err := json.Unmarshal(record, &notice); err != nil {
			return err
		}
		release, ok := notice.Releases[h.Codename]
		if !ok {
			return nil
		}
		releaseSeen = true
		for name, binary := range release.Binaries {
			for _, p := range byName[name] {
				if binary.Version == "" {
					continue
				}
				c, e := CompareVersions(ctx, s.Runner, "apt", p.Version, binary.Version)
				if e != nil {
					return e
				}
				if c >= 0 {
					continue
				}
				for _, cve := range CVEs(strings.Join(notice.CVEs, " ")) {
					out = append(out, finding(h, p, cve, "USN-"+id, binary.Version, "unknown", "confirmed", "Ubuntu notice lists a fixed binary release for "+h.Codename, source))
				}
			}
		}
		return nil
	})
	if err == nil && !releaseSeen {
		err = errors.New("Ubuntu notices do not cover this release")
	}
	return out, []Source{source}, err
}

type rpmUpdate struct {
	Type       string `xml:"type,attr"`
	ID         string `xml:"id"`
	Severity   string `xml:"severity"`
	References []struct {
		Type string `xml:"type,attr"`
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	} `xml:"references>reference"`
	Packages []struct {
		Name    string `xml:"name,attr"`
		Version string `xml:"version,attr"`
		Release string `xml:"release,attr"`
		Epoch   string `xml:"epoch,attr"`
		Arch    string `xml:"arch,attr"`
	} `xml:"pkglist>collection>package"`
}

func (i *Intelligence) rpmAdvisories(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, error) {
	dirs := []string{"var/cache/dnf", "var/cache/libdnf5"}
	if h.Manager == "zypper" {
		dirs = []string{"var/cache/zypp/raw"}
	}
	byName := map[string][]Package{}
	for _, p := range packages {
		byName[p.Name] = append(byName[p.Name], p)
	}
	out := []Finding{}
	sources := []Source{}
	visited := 0
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(s.Root, dir), func(path string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if os.IsNotExist(walkErr) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			visited++
			if visited > 20000 {
				return errors.New("repository metadata exceeds traversal limit")
			}
			if entry.IsDir() || !strings.Contains(entry.Name(), "updateinfo") || (!strings.HasSuffix(path, ".xml") && !strings.HasSuffix(path, ".xml.gz")) {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			var reader io.Reader = f
			if strings.HasSuffix(path, ".gz") {
				z, e := gzip.NewReader(f)
				if e != nil {
					return e
				}
				defer z.Close()
				reader = z
			}
			info, e := entry.Info()
			if e != nil {
				return e
			}
			source := Source{Name: h.ID + "-updateinfo", URL: "native-repository-cache", RetrievedAt: info.ModTime(), Stale: true}
			sources = append(sources, source)
			d := xml.NewDecoder(io.LimitReader(reader, 64<<20))
			for {
				token, e := d.Token()
				if errors.Is(e, io.EOF) {
					break
				}
				if e != nil {
					return e
				}
				start, ok := token.(xml.StartElement)
				if !ok || start.Name.Local != "update" {
					continue
				}
				var update rpmUpdate
				if err = d.DecodeElement(&update, &start); err != nil {
					return err
				}
				if update.Type != "security" {
					continue
				}
				for _, fixed := range update.Packages {
					for _, p := range byName[fixed.Name] {
						if fixed.Arch != "" && fixed.Arch != p.Architecture {
							continue
						}
						version := fixed.Version + "-" + fixed.Release
						if fixed.Epoch != "" && fixed.Epoch != "0" {
							version = fixed.Epoch + ":" + version
						}
						c, e := CompareVersions(ctx, s.Runner, h.Manager, p.Version, version)
						if e != nil {
							return e
						}
						if c >= 0 {
							continue
						}
						for _, ref := range update.References {
							for _, cve := range CVEs(ref.ID) {
								f := finding(h, p, cve, update.ID, version, update.Severity, "potential", "cached repository advisory requires current repository verification", source)
								if link := safeURL(ref.Href); link != "" {
									f.References = append(f.References, link)
								}
								out = append(out, f)
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return out, sources, err
		}
	}
	if len(sources) == 0 {
		return out, sources, fmt.Errorf("%s security repository metadata is unavailable; refresh native repository metadata before scanning", h.Manager)
	}
	return out, sources, errors.New("cached repository metadata cannot establish complete current coverage")
}
