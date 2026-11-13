package hostsecurity

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

func CompareVersions(ctx context.Context, r Runner, manager, a, b string) (int, error) {
	if !packageVersion.MatchString(a) || !packageVersion.MatchString(b) {
		return 0, errors.New("invalid distribution package version")
	}
	switch manager {
	case "apt":
		return debianCompare(a, b), nil
	case "dnf", "zypper":
		return rpmCompare(a, b), nil
	case "apk":
		raw, err := r.Run(ctx, "apk", "version", "-t", a, b)
		if err != nil {
			return 0, err
		}
		switch strings.TrimSpace(string(raw)) {
		case "<":
			return -1, nil
		case "=":
			return 0, nil
		case ">":
			return 1, nil
		}
		return 0, errors.New("invalid APK version comparison")
	case "pacman":
		raw, err := r.Run(ctx, "vercmp", a, b)
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return 0, errors.New("invalid Pacman version comparison")
		}
		return sign(n), nil
	default:
		return 0, errors.New("unsupported version comparison")
	}
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}
func digit(c byte) bool { return c >= '0' && c <= '9' }
func alpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func evr(v string) (string, string, string) {
	epoch := "0"
	if a, b, ok := strings.Cut(v, ":"); ok {
		epoch = a
		v = b
	}
	release := ""
	if i := strings.LastIndex(v, "-"); i >= 0 {
		release = v[i+1:]
		v = v[:i]
	}
	return epoch, v, release
}

func numberCompare(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return sign(len(a) - len(b))
	}
	return strings.Compare(a, b)
}

func debianCompare(a, b string) int {
	ae, av, ar := evr(a)
	be, bv, br := evr(b)
	if c := numberCompare(ae, be); c != 0 {
		return c
	}
	if c := debianPart(av, bv); c != 0 {
		return c
	}
	if ar == "" {
		ar = "0"
	}
	if br == "" {
		br = "0"
	}
	return debianPart(ar, br)
}

func debianOrder(v string) int {
	if len(v) == 0 {
		return 0
	}
	c := v[0]
	if c == '~' {
		return -1
	}
	if digit(c) {
		return 0
	}
	if alpha(c) {
		return int(c)
	}
	return int(c) + 256
}

func debianPart(a, b string) int {
	for len(a) > 0 || len(b) > 0 {
		for len(a) > 0 && !digit(a[0]) || len(b) > 0 && !digit(b[0]) {
			if x, y := debianOrder(a), debianOrder(b); x != y {
				return sign(x - y)
			}
			if len(a) > 0 {
				a = a[1:]
			}
			if len(b) > 0 {
				b = b[1:]
			}
		}
		i, j := 0, 0
		for i < len(a) && digit(a[i]) {
			i++
		}
		for j < len(b) && digit(b[j]) {
			j++
		}
		if c := numberCompare(a[:i], b[:j]); c != 0 {
			return c
		}
		a = a[i:]
		b = b[j:]
	}
	return 0
}

func rpmCompare(a, b string) int {
	ae, av, ar := evr(a)
	be, bv, br := evr(b)
	if c := numberCompare(ae, be); c != 0 {
		return c
	}
	if c := rpmPart(av, bv); c != 0 {
		return c
	}
	return rpmPart(ar, br)
}

func rpmPart(a, b string) int {
	for len(a) > 0 || len(b) > 0 {
		for len(a) > 0 && !alpha(a[0]) && !digit(a[0]) && a[0] != '~' && a[0] != '^' {
			a = a[1:]
		}
		for len(b) > 0 && !alpha(b[0]) && !digit(b[0]) && b[0] != '~' && b[0] != '^' {
			b = b[1:]
		}
		if strings.HasPrefix(a, "~") || strings.HasPrefix(b, "~") {
			if !strings.HasPrefix(a, "~") {
				return 1
			}
			if !strings.HasPrefix(b, "~") {
				return -1
			}
			a = a[1:]
			b = b[1:]
			continue
		}
		if strings.HasPrefix(a, "^") || strings.HasPrefix(b, "^") {
			if a == "" {
				return -1
			}
			if b == "" {
				return 1
			}
			if a[0] != '^' {
				return 1
			}
			if b[0] != '^' {
				return -1
			}
			a = a[1:]
			b = b[1:]
			continue
		}
		if a == "" || b == "" {
			return sign(len(a) - len(b))
		}
		numeric := digit(a[0])
		i, j := 0, 0
		for i < len(a) && ((numeric && digit(a[i])) || (!numeric && alpha(a[i]))) {
			i++
		}
		for j < len(b) && ((numeric && digit(b[j])) || (!numeric && alpha(b[j]))) {
			j++
		}
		if j == 0 {
			if numeric {
				return 1
			}
			return -1
		}
		var c int
		if numeric {
			c = numberCompare(a[:i], b[:j])
		} else {
			c = strings.Compare(a[:i], b[:j])
		}
		if c != 0 {
			return c
		}
		a = a[i:]
		b = b[j:]
	}
	return 0
}
