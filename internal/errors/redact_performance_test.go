package phelixerr

import (
	"strings"
	"testing"
)

func BenchmarkRedact(b *testing.B) {
	for name, line := range map[string]string{
		"plain":       "application handled request successfully",
		"uppercase":   "APPLICATION HANDLED REQUEST SUCCESSFULLY",
		"credentials": "postgres://user:private@localhost/db token=private Bearer private",
		"long":        strings.Repeat("application handled request successfully ", 200),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = Redact(line)
			}
		})
	}
}
