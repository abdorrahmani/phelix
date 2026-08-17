package auth

import (
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestParseExpiry(t *testing.T) {
	t.Run("parses RFC3339 value", func(t *testing.T) {
		got, err := parseExpiry("2026-11-14T09:30:00Z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("parseExpiry = %v, want %v", got, want)
		}
	})

	t.Run("empty value falls back to default TTL", func(t *testing.T) {
		before := time.Now()
		got, err := parseExpiry("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		after := time.Now()
		if got.Before(before.Add(defaultSessionTTL)) || got.After(after.Add(defaultSessionTTL)) {
			t.Errorf("parseExpiry(\"\") = %v, want roughly now+defaultSessionTTL", got)
		}
	})

	t.Run("malformed value returns error", func(t *testing.T) {
		if _, err := parseExpiry("not-a-time"); err == nil {
			t.Fatal("expected error for malformed expiresAt, got nil")
		}
	})
}

func TestAuthHeaders(t *testing.T) {
	h := authHeaders()

	// Mandatory header must always be present.
	if v := h["X-CLI-Version"]; v == "" {
		t.Error("X-CLI-Version header missing or empty")
	}

	// OS and architecture come from the runtime and must be present.
	if h["X-OS"] == "" || h["X-Architecture"] == "" {
		t.Errorf("runtime headers missing: %v", h)
	}
}

func TestProcessUptime(t *testing.T) {
	// processUptime may be empty when the process started < 1s ago, but it must
	// never be garbage.
	if up := processUptime(); up != "" {
		if _, err := time.ParseDuration(up); err != nil {
			t.Errorf("processUptime() = %q is not a valid duration: %v", up, err)
		}
	}
}

func TestStoreSessionPersistsExpiry(t *testing.T) {
	// Round-trip the session through storeSession/readSession using the
	// documented fields, verifying the server-provided expiry survives.
	exp := time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	s := &Session{
		SessionID: "a1b2c3d4-1111-2222-3333-444455556666",
		Token:     "eyJhbGciOiJIUzI1NiIs…",
		Username:  "abdul",
		UserID:    7,
		ExpiresAt: exp,
	}

	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := storeSession(s); err != nil {
		t.Fatalf("storeSession: %v", err)
	}
	got, err := readSession()
	if err != nil {
		t.Fatalf("readSession: %v", err)
	}
	if got.SessionID != s.SessionID {
		t.Errorf("sessionID = %q, want %q", got.SessionID, s.SessionID)
	}
	if got.Username != "abdul" {
		t.Errorf("username = %q, want abdul", got.Username)
	}
	if got.UserID != 7 {
		t.Errorf("userID = %d, want 7", got.UserID)
	}
	if !got.ExpiresAt.Equal(exp) {
		t.Errorf("expiresAt = %v, want %v", got.ExpiresAt, exp)
	}
}

func TestGetValidSessionExpiryCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	s := &Session{
		SessionID: "sess-1",
		Token:     "tok",
		ExpiresAt: time.Now().Add(-time.Hour), // already expired
	}
	if err := storeSession(s); err != nil {
		t.Fatalf("storeSession: %v", err)
	}

	_, err := GetValidSession()
	if err == nil {
		t.Fatal("expected error for expired session, got nil")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeSessionExpired) {
		t.Errorf("expected CodeSessionExpired, got %v", phelixerr.CodeOf(err))
	}
}
