package app

import "regexp"

// appIDPattern matches the RFC 4122 version 4 UUID shape GenerateAppID
// produces. It is the single validator for persisted application identifiers
// so every consumer accepts exactly the same shape.
var appIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidAppID reports whether id has the shape of a generated application ID.
// Callers use it before building file paths from an identifier, so a foreign
// or hostile string can never escape its directory.
func ValidAppID(id string) bool {
	return appIDPattern.MatchString(id)
}
