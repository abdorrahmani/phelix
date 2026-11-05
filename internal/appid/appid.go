package appid

import "regexp"

var pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Valid reports whether id is a generated application identifier.
func Valid(id string) bool {
	return pattern.MatchString(id)
}
