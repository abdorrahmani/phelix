package phelixerr

import "testing"

func TestRedactPreservesUnicodeAroundCredentials(t *testing.T) {
	for input, want := range map[string]string{
		"İ token=private":                "İ ***",
		"Ⱥ token=private":                "Ⱥ ***",
		"İȺ token=private":               "İȺ ***",
		"K token=private and sk-private": "K *** and ***",
		"İ TOKEN=private":                "İ ***",
		"K sk-":                          "K ***",
		"sK-private":                     "***",
		"token=first token=second":       "*** ***",
	} {
		if got := Redact(input); got != want {
			t.Errorf("Redact(%q) = %q, want %q", input, got, want)
		}
	}
}
