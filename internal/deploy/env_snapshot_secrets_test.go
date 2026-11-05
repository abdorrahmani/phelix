package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	envpkg "github.com/abdorrahmani/phelix/internal/env"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

const secretsTestAppID = "1f0e1d2c-3b4a-4958-8677-8899aabbccdd"

func secretsTestEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(root, "data"))
	return root
}

func writeSnapshot(t *testing.T, appID string, entries map[string]string) string {
	t.Helper()
	master, err := envpkg.GetMasterKey()
	if err != nil {
		t.Fatalf("master key: %v", err)
	}
	store := envpkg.EnvStore{Version: 1, AppID: appID, Entries: map[string]envpkg.EnvEntry{}}
	for k, v := range entries {
		enc, err := envpkg.EncryptData(v, master)
		if err != nil {
			t.Fatalf("encrypt %s: %v", k, err)
		}
		store.Entries[k] = envpkg.EnvEntry{Key: k, Value: enc}
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "v1.enc")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return path
}

func TestEnvOverlayWithoutSecretsUnchanged(t *testing.T) {
	secretsTestEnv(t)
	path := writeSnapshot(t, secretsTestAppID, map[string]string{"PLAIN_VAR": "plain-value"})
	overlay, err := EnvOverlayFromSnapshot(path, secretsTestAppID)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if len(overlay) != 1 || overlay[0] != "PLAIN_VAR=plain-value" {
		t.Fatalf("overlay = %v", overlay)
	}
}

func TestEnvOverlayMergesSecretsByKey(t *testing.T) {
	secretsTestEnv(t)
	if _, err := secrets.Set(secretsTestAppID, "database_url", "postgres://fresh"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := secrets.ReplaceBindings(secretsTestAppID, []secrets.Binding{{Secret: "database_url", Env: "DATABASE_URL"}}); err != nil {
		t.Fatalf("bindings: %v", err)
	}
	path := writeSnapshot(t, secretsTestAppID, map[string]string{
		"DATABASE_URL": "postgres://stale-snapshot-value",
		"PLAIN_VAR":    "plain-value",
	})
	overlay, err := EnvOverlayFromSnapshot(path, secretsTestAppID)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	joined := strings.Join(overlay, "\n")
	if strings.Contains(joined, "stale-snapshot-value") {
		t.Fatalf("stale snapshot value must be replaced, got %v", overlay)
	}
	if strings.Contains(joined, "postgres://fresh,") || strings.Count(joined, "DATABASE_URL=") != 1 {
		t.Fatalf("managed name must appear exactly once, got %v", overlay)
	}
	found := 0
	for _, kv := range overlay {
		if kv == "DATABASE_URL=postgres://fresh" {
			found++
		}
		if kv == "PLAIN_VAR=plain-value" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("overlay = %v", overlay)
	}
}

func TestEnvOverlayWithCandidateBindings(t *testing.T) {
	secretsTestEnv(t)
	if _, err := secrets.Set(secretsTestAppID, "api_key", "candidate-key-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := secrets.ReplaceBindings(secretsTestAppID, nil); err != nil {
		t.Fatalf("clear committed bindings: %v", err)
	}
	path := writeSnapshot(t, secretsTestAppID, nil)
	candidates := []secrets.Binding{{Secret: "api_key", Env: "API_KEY"}}
	overlay, err := EnvOverlayWithBindings(path, secretsTestAppID, candidates)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if len(overlay) != 1 || overlay[0] != "API_KEY=candidate-key-1" {
		t.Fatalf("overlay = %v", overlay)
	}
}

func TestSnapshotNeverContainsSecretValues(t *testing.T) {
	secretsTestEnv(t)
	secretValue := "definitely-should-not-appear-anywhere-7"
	if _, err := secrets.Set(secretsTestAppID, "db", secretValue); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := secrets.ReplaceBindings(secretsTestAppID, []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}); err != nil {
		t.Fatalf("bindings: %v", err)
	}
	snapshotPath, err := snapshotEnvForVersion("secrets-app", secretsTestAppID, 1)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(raw), secretValue) {
		t.Fatal("the version snapshot must never contain a secret value")
	}
	overlay, err := EnvOverlayFromSnapshot(snapshotPath, secretsTestAppID)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	joined := strings.Join(overlay, "\n")
	if !strings.Contains(joined, secretValue) {
		t.Fatalf("the launch overlay must resolve the secret from the store, got %v", overlay)
	}
}

func TestMergeByKeyUnit(t *testing.T) {
	cases := []struct {
		name    string
		base    []string
		overlay []string
		want    []string
	}{
		{
			name:    "overlay replaces base",
			base:    []string{"A=1", "B=2"},
			overlay: []string{"B=3"},
			want:    []string{"A=1", "B=3"},
		},
		{
			name:    "overlay appends new names",
			base:    []string{"A=1"},
			overlay: []string{"C=4", "B=2"},
			want:    []string{"A=1", "C=4", "B=2"},
		},
		{
			name:    "deduplicates repeated base keys",
			base:    []string{"A=1", "A=2", "B=3"},
			overlay: []string{"C=4"},
			want:    []string{"A=2", "B=3", "C=4"},
		},
		{
			name:    "malformed entries pass through",
			base:    []string{"A=1", "malformed"},
			overlay: []string{"B=2"},
			want:    []string{"A=1", "malformed", "B=2"},
		},
		{
			name:    "empty overlay returns base",
			base:    []string{"A=1"},
			overlay: nil,
			want:    []string{"A=1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeByKey(tc.base, tc.overlay)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

func TestMissingDeclaredSecretFailsLaunch(t *testing.T) {
	secretsTestEnv(t)
	if _, err := secrets.Set(secretsTestAppID, "ghost", "value"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.ReplaceBindings(secretsTestAppID, []secrets.Binding{{Secret: "ghost", Env: "GHOST"}}); err != nil {
		t.Fatal(err)
	}
	st, err := secrets.Open(secretsTestAppID)
	if err != nil {
		t.Fatal(err)
	}
	delete(st.Secrets, "ghost")
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "secrets", secretsTestAppID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeSnapshot(t, secretsTestAppID, nil)
	if _, err := EnvOverlayFromSnapshot(path, secretsTestAppID); err == nil {
		t.Fatal("a declared but missing secret must fail the launch overlay")
	}
}
