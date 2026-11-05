// Package appid holds the safety contract for Phelix application
// identifiers used in file paths. It lives in its own small package so every
// consumer, including packages the deploy engine depends on, can validate an
// identifier without creating an import cycle.
//
// Phelix produces two identifier shapes: RFC 4122 version 4 UUIDs from the
// application manager, and short names or numbers used by workers and tests.
// Both are safe inside a path. Anything that could escape its directory is
// not, and this package is the single place that decides.
package appid

// maxIDLength bounds every identifier shape the system produces.
const maxIDLength = 64

// Valid reports whether id is safe to build a file path from. It accepts the
// identifier shapes Phelix generates (UUIDs, names, numbers) and refuses
// anything containing path separators, dot components or characters outside
// the set the rest of the runtime already accepts for app names.
func Valid(id string) bool {
	if id == "" || len(id) > maxIDLength {
		return false
	}
	if id == "." || id == ".." {
		return false
	}
	if id[0] == '.' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}
