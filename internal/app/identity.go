package app

import "github.com/abdorrahmani/phelix/internal/appid"

// ValidAppID reports whether id has the shape of a generated application ID.
// It delegates to the shared validator so every consumer accepts exactly the
// same shape.
func ValidAppID(id string) bool {
	return appid.Valid(id)
}
