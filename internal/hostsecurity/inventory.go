package hostsecurity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var packageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9+_.:-]{0,255}$`)
var packageVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.+_:~^\-]{0,255}$`)

func ValidPackageName(name string) bool { return packageName.MatchString(name) }

func normalizePackage(p Package, manager string, at time.Time) (Package, error) {
	if !ValidPackageName(p.Name) || !packageVersion.MatchString(p.Version) || len(p.Architecture) > 64 {
		return p, errors.New("invalid package identity in database")
	}
	if p.Architecture == "" {
		p.Architecture = "unknown"
	}
	if p.Source == "" {
		p.Source = p.Name
	}
	if p.SourceVersion == "" {
		p.SourceVersion = p.Version
	}
	if p.Repository == "" {
		p.Repository = "unknown"
	}
	if p.Origin == "" {
		p.Origin = "unknown"
	}
	if p.Held == "" {
		p.Held = "unknown"
	}
	p.Key = p.Name + ":" + p.Architecture + "@" + p.Version
	p.Manager = manager
	p.Status = "installed"
	p.InventoryAt = at
	if i := strings.LastIndex(p.Version, "-"); i >= 0 {
		p.Release = p.Version[i+1:]
	}
	return p, nil
}

func ParseDPKG(raw []byte) ([]Package, error) {
	var out []Package
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 9 {
			return nil, errors.New("invalid dpkg inventory record")
		}
		if len(f[0]) < 3 {
			return nil, errors.New("invalid dpkg package status")
		}
		if f[0][1] != 'i' {
			continue
		}
		hold := "no"
		if f[0][0] == 'h' {
			hold = "yes"
		}
		out = append(out, Package{Name: f[1], Version: f[2], Architecture: f[3], Source: f[4], SourceVersion: f[5], Origin: f[6], Held: hold, Provides: splitList(f[7]), Depends: splitList(f[8])})
	}
	return out, nil
}

func ParseRPM(raw []byte) ([]Package, error) {
	var out []Package
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			return nil, errors.New("invalid RPM inventory record")
		}
		version := f[2] + "-" + f[3]
		if f[1] != "0" && f[1] != "(none)" && f[1] != "" {
			version = f[1] + ":" + version
		}
		source := strings.TrimSuffix(f[5], ".src.rpm")
		sv := ""
		parts := strings.Split(source, "-")
		if len(parts) >= 3 {
			source = strings.Join(parts[:len(parts)-2], "-")
			sv = strings.Join(parts[len(parts)-2:], "-")
		}
		if f[5] == "(none)" {
			source = ""
		}
		out = append(out, Package{Name: f[0], Version: version, Architecture: f[4], Source: source, SourceVersion: sv, Origin: f[6]})
	}
	return out, nil
}

func ParseAPK(raw []byte) ([]Package, error) {
	var out []Package
	for block := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n\n") {
		if block == "" {
			continue
		}
		v := map[string]string{}
		for line := range strings.SplitSeq(block, "\n") {
			k, val, ok := strings.Cut(line, ":")
			if !ok {
				return nil, errors.New("invalid APK inventory record")
			}
			v[k] = val
		}
		if v["P"] == "" || v["V"] == "" {
			return nil, errors.New("APK record missing package identity")
		}
		out = append(out, Package{Name: v["P"], Version: v["V"], Architecture: v["A"], Source: v["o"], Origin: v["m"], Provides: strings.Fields(v["p"]), Depends: strings.Fields(v["D"])})
	}
	return out, nil
}

func ParsePacman(raw []byte) (Package, error) {
	v := map[string][]string{}
	key := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "%") && strings.HasSuffix(line, "%") {
			key = strings.Trim(line, "%")
			continue
		}
		if line != "" && key != "" {
			v[key] = append(v[key], line)
		}
	}
	first := func(k string) string {
		if len(v[k]) > 0 {
			return v[k][0]
		}
		return ""
	}
	p := Package{Name: first("NAME"), Version: first("VERSION"), Architecture: first("ARCH"), Source: first("BASE"), Origin: first("PACKAGER"), Provides: v["PROVIDES"], Depends: v["DEPENDS"]}
	if p.Name == "" || p.Version == "" {
		return p, errors.New("Pacman record missing package identity")
	}
	return p, nil
}

func splitList(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ", ")
}

func (s System) Inventory(ctx context.Context, h Host, at time.Time) ([]Package, string, error) {
	if h.Status != "supported" {
		return nil, "", fmt.Errorf("inventory %s: %s", h.Status, h.Reason)
	}
	var packages []Package
	var raw []byte
	var err error
	switch h.Manager {
	case "apt":
		raw, err = s.Runner.Run(ctx, "dpkg-query", "-W", "-f=${db:Status-Abbrev}\t${Package}\t${Version}\t${Architecture}\t${source:Package}\t${source:Version}\t${Origin}\t${Provides}\t${Depends}\n")
		if err == nil {
			packages, err = ParseDPKG(raw)
		}
	case "dnf", "zypper":
		raw, err = s.Runner.Run(ctx, "rpm", "-qa", "--qf", "%{NAME}\t%{EPOCHNUM}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{SOURCERPM}\t%{VENDOR}\n")
		if err == nil {
			packages, err = ParseRPM(raw)
		}
	case "apk":
		raw, err = s.read("lib/apk/db/installed")
		if err == nil {
			packages, err = ParseAPK(raw)
		}
	case "pacman":
		var dir *os.File
		dir, err = os.Open(filepath.Join(s.Root, "var/lib/pacman/local"))
		if err != nil {
			break
		}
		defer dir.Close()
		for err == nil {
			if err = ctx.Err(); err != nil {
				break
			}
			var entries []os.DirEntry
			entries, err = dir.ReadDir(128)
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				var b []byte
				b, err = s.read(filepath.Join("var/lib/pacman/local", entry.Name(), "desc"))
				if err != nil {
					return nil, "", fmt.Errorf("cannot read Pacman database: %w", err)
				}
				p, e := ParsePacman(b)
				if e != nil {
					return nil, "", e
				}
				packages = append(packages, p)
				if len(packages) > MaxPackages {
					return nil, "", errors.New("inventory exceeds package limit")
				}
			}
			if errors.Is(err, io.EOF) {
				err = nil
				break
			}
		}
	default:
		return nil, "", errors.New("unsupported package manager")
	}
	if err != nil {
		return nil, "", err
	}
	if err = ctx.Err(); err != nil {
		return nil, "", err
	}
	if len(packages) == 0 {
		return nil, "", errors.New("package database returned no installed packages")
	}
	if len(packages) > MaxPackages {
		return nil, "", errors.New("inventory exceeds package limit")
	}
	for i, p := range packages {
		packages[i], err = normalizePackage(p, h.Manager, at)
		if err != nil {
			return nil, "", err
		}
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Key < packages[j].Key })
	hash := sha256.New()
	for i, p := range packages {
		if i > 0 && packages[i-1].Key == p.Key {
			return nil, "", errors.New("duplicate package identity in inventory")
		}
		p.InventoryAt = time.Time{}
		data, _ := json.Marshal(p)
		hash.Write(data)
	}
	return packages, hex.EncodeToString(hash.Sum(nil)), nil
}
