package appname

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"regexp"
)

var pattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

func Validate(name string) error {
	if !pattern.MatchString(name) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid application name %q: use letters, digits, dot, dash or underscore (1-64 characters, no path separators)", name)
	}
	return nil
}
