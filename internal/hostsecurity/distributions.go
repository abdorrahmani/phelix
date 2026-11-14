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

// debianRelease is one release-specific advisory entry from the tracker.
type debianRelease struct {
	Status  string `json:"status"`
	Fixed   string `json:"fixed_version"`
	Urgency string `json:"urgency"`
}

// versionComparable reports whether a feed-supplied fixed version is a real
// Debian-family version the comparator accepts. Debian versions start with
// a digit, so the "0" placeholder and word values never reach a comparison.
func versionComparable(value string) bool {
	return value != "" && value != "0" && digit(value[0]) && packageVersion.MatchString(value)
}

// debianSeverity normalizes the tracker urgency onto the scan severity
// vocabulary.
func debianSeverity(urgency string) string {
	severity := strings.TrimSuffix(urgency, "**")
	if severity == "unimportant" {
		severity = "low"
	}
	return severity
}

// debianVerdict resolves one tracker entry into the scan action it supports.
// An empty state means the package is not affected and no finding is due.
// Comparisons run only for recognized statuses carrying a real fixed
// version; the no-fix placeholder and unknown vocabulary degrade to an
// unverified finding and never reach the comparator.
func debianVerdict(status debianRelease, release string) (state, reason string, compare bool) {
	switch status.Status {
	case "not-affected":
		return "", "", false
	case "resolved":
		if status.Fixed == "0" {
			return "", "", false
		}
		if versionComparable(status.Fixed) {
			return "confirmed", "installed Debian source version is below the release-specific fixed version", true
		}
		return "unverified", "Debian tracker resolution carries no usable fixed version", false
	case "open", "undetermined":
		if status.Fixed == "0" {
			return "unverified", "Debian tracker reports the source package affected with no fixed version available for " + release, false
		}
		if versionComparable(status.Fixed) {
			return "confirmed", "installed Debian source version is below the release-specific fixed version", true
		}
		if status.Fixed == "" && status.Status == "open" {
			return "confirmed", "Debian tracker marks this source package open for " + release, false
		}
		return "unverified", "Debian tracker cannot establish an affected version", false
	}
	return "unverified", "Debian tracker status is outside the recognized vocabulary", false
}

func (i *Intelligence) debian(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, int, error) {
	raw, source, err := i.fetch(ctx, "debian", "https://security-tracker.debian.org/tracker/data/json")
	if err != nil {
		return nil, []Source{source}, 0, err
	}
	release := h.Codename
	if release == "" {
		release = map[string]string{"10": "buster", "11": "bullseye", "12": "bookworm", "13": "trixie", "14": "forky"}[h.Version]
	}
	if release == "" {
		return nil, []Source{source}, 0, errors.New("Debian release is unknown")
	}
	bySource := packageSources(packages)
	out := []Finding{}
	releaseSeen := false
	covered := map[string]bool{}
	err = decodeObject(raw, false, func(name string, record json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		targets := bySource[name]
		if len(targets) == 0 {
			return nil
		}
		var advisories map[string]struct {
			Description string                   `json:"description"`
			Releases    map[string]debianRelease `json:"releases"`
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
			for _, p := range targets {
				covered[p.Key] = true
			}
			if len(CVEs(cve)) != 1 {
				continue
			}
			state, reason, compare := debianVerdict(status, release)
			if state == "" {
				continue
			}
			severity := debianSeverity(status.Urgency)
			if compare {
				for _, p := range targets {
					comparison, e := CompareVersions(ctx, s.Runner, "apt", p.SourceVersion, status.Fixed)
					if e != nil {
						return e
					}
					if comparison >= 0 {
						continue
					}
					out = append(out, finding(h, p, cve, cve, status.Fixed, severity, "confirmed", reason, source))
				}
				continue
			}
			for _, p := range targets {
				out = append(out, finding(h, p, cve, cve, "", severity, state, reason, source))
			}
		}
		return nil
	})
	if err == nil && !releaseSeen {
		err = errors.New("no Debian release-specific package coverage found")
	}
	unknown := 0
	if err == nil {
		for _, p := range packages {
			if !covered[p.Key] {
				unknown++
			}
		}
	}
	return out, []Source{source}, unknown, err
}

func (i *Intelligence) alpine(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, int, error) {
	parts := strings.Split(h.Version, ".")
	if len(parts) < 2 {
		return nil, nil, 0, errors.New("Alpine release is unknown")
	}
	release := "v" + strings.Join(parts[:2], ".")
	out := []Finding{}
	sources := []Source{}
	var failures []error
	bySource := packageSources(packages)
	covered := map[string]bool{}
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
				covered[p.Key] = true
				for fixed, ids := range entry.Pkg.Fixes {
					comparison, e := CompareVersions(ctx, s.Runner, "apk", p.Version, fixed)
					if e != nil {
						return out, sources, 0, e
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
	err := errors.Join(failures...)
	unknown := 0
	if err == nil {
		for _, p := range packages {
			if !covered[p.Key] {
				unknown++
			}
		}
	}
	return out, sources, unknown, err
}

func (i *Intelligence) arch(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, int, error) {
	raw, source, err := i.fetch(ctx, "arch", "https://security.archlinux.org/all.json")
	if err != nil {
		return nil, []Source{source}, 0, err
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
		return nil, []Source{source}, 0, err
	}
	byName := map[string][]Package{}
	for _, p := range packages {
		byName[p.Name] = append(byName[p.Name], p)
	}
	out := []Finding{}
	covered := map[string]bool{}
	for _, record := range records {
		for _, name := range record.Packages {
			for _, p := range byName[name] {
				covered[p.Key] = true
				state := "confirmed"
				reason := "Arch tracker identifies an affected package release"
				if record.Fixed != "" {
					c, e := CompareVersions(ctx, s.Runner, "pacman", p.Version, record.Fixed)
					if e != nil {
						return out, []Source{source}, 0, e
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
	unknown := 0
	if err == nil {
		for _, p := range packages {
			if !covered[p.Key] {
				unknown++
			}
		}
	}
	return out, []Source{source}, unknown, nil
}

func (i *Intelligence) ubuntu(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, int, error) {
	raw, source, err := i.fetch(ctx, "ubuntu", "https://usn.ubuntu.com/usn-db/database.json.bz2")
	if err != nil {
		return nil, []Source{source}, 0, err
	}
	if h.Codename == "" {
		return nil, []Source{source}, 0, errors.New("Ubuntu release codename is missing")
	}
	byName := map[string][]Package{}
	for _, p := range packages {
		byName[p.Name] = append(byName[p.Name], p)
	}
	out := []Finding{}
	releaseSeen := false
	covered := map[string]bool{}
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
				if !versionComparable(binary.Version) {
					continue
				}
				covered[p.Key] = true
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
	unknown := 0
	if err == nil {
		for _, p := range packages {
			if !covered[p.Key] {
				unknown++
			}
		}
	}
	return out, []Source{source}, unknown, err
}

type rpmUpdate struct {
	Type       string `xml:"type,attr"`
	ID         string `xml:"id,attr"`
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

func (i *Intelligence) rpmAdvisories(ctx context.Context, s System, h Host, packages []Package) ([]Finding, []Source, int, error) {
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
								f := finding(h, p, cve, update.ID, version, normalizeAdvisorySeverity(update.Severity), "potential", "cached repository advisory requires current repository verification", source)
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
			return out, sources, 0, err
		}
	}
	if len(sources) == 0 {
		return out, sources, 0, fmt.Errorf("%s security repository metadata is unavailable; refresh native repository metadata before scanning", h.Manager)
	}
	return out, sources, 0, errors.New("cached repository metadata cannot establish complete current coverage")
}
