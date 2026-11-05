package phelixerr

import (
	"strings"
	"sync"
	"testing"
)

func TestRegisteredValueMaskedAndReleased(t *testing.T) {
	RegisterSecretValue("s3cret-database-password")
	if got := Redact("connecting with s3cret-database-password now"); strings.Contains(got, "s3cret") {
		t.Fatalf("registered value leaked: %q", got)
	}
	if got := Redact("connecting with s3cret-database-password now"); got != "connecting with *** now" {
		t.Fatalf("unexpected masking: %q", got)
	}
	UnregisterSecretValue("s3cret-database-password")
	if got := Redact("connecting with s3cret-database-password now"); !strings.Contains(got, "s3cret-database-password") {
		t.Fatalf("released value should render normally again: %q", got)
	}
}

func TestReferenceCountingKeepsSharedValuesMasked(t *testing.T) {
	value := "shared-credential-42"
	RegisterSecretValue(value)
	RegisterSecretValue(value)
	UnregisterSecretValue(value)
	if got := Redact("token " + value + " end"); strings.Contains(got, value) {
		t.Fatalf("value must stay masked while another owner holds it: %q", got)
	}
	UnregisterSecretValue(value)
	if got := Redact("token " + value + " end"); !strings.Contains(got, value) {
		t.Fatalf("value must render again after the last owner releases it: %q", got)
	}
}

func TestLongerValueWinsOverPrefix(t *testing.T) {
	RegisterSecretValue("supersecret")
	RegisterSecretValue("supersecret123")
	if got := Redact("x supersecret123 y"); got != "x *** y" {
		t.Fatalf("longer value must be masked whole, got %q", got)
	}
	if got := Redact("x supersecret y"); got != "x *** y" {
		t.Fatalf("shorter value must still mask, got %q", got)
	}
	UnregisterSecretValue("supersecret")
	UnregisterSecretValue("supersecret123")
}

func TestEmptyValueIgnored(t *testing.T) {
	before := registeredValueCount()
	RegisterSecretValue("")
	UnregisterSecretValue("")
	if got := registeredValueCount(); got != before {
		t.Fatalf("empty values must not enter the registry: %d -> %d", before, got)
	}
}

func TestUnknownUnregisterIsHarmless(t *testing.T) {
	before := registeredValueCount()
	UnregisterSecretValue("never-registered-value")
	if got := registeredValueCount(); got != before {
		t.Fatalf("releasing an unknown value must not change the registry: %d -> %d", before, got)
	}
}

func TestRegistrySurvives(t *testing.T) {
	RegisterSecretValue("sk-test-abc123")
	if got := Redact("key sk-test-abc123 rest"); strings.Contains(got, "sk-test-abc123") {
		t.Fatal("registered values must be masked even when they also match builtin patterns")
	}
	UnregisterSecretValue("sk-test-abc123")
}

func TestConcurrentRegisterRedactUnregister(t *testing.T) {
	values := make([]string, 16)
	for i := range values {
		values[i] = "concurrent-secret-" + string(rune('a'+i)) + "-value"
	}
	var wg sync.WaitGroup
	for round := 0; round < 4; round++ {
		for _, v := range values {
			wg.Add(3)
			go func(v string) {
				defer wg.Done()
				RegisterSecretValue(v)
			}(v)
			go func(v string) {
				defer wg.Done()
				_ = Redact("log line containing " + v + " and more text")
			}(v)
			go func(v string) {
				defer wg.Done()
				UnregisterSecretValue(v)
			}(v)
		}
	}
	wg.Wait()
	for _, v := range values {
		RegisterSecretValue(v)
		defer UnregisterSecretValue(v)
	}
}

func TestFallbackMasksInvalidBytesAndOverlappingValues(t *testing.T) {
	values := []string{"fallback-\xff", "fallback-\xff-long\nsecond-line"}
	release := AcquireSecretValues(values)
	defer release()
	if got := Redact("before " + values[1] + " after"); got != "before *** after" {
		t.Fatalf("fallback left secret fragments: %q", got)
	}
}
