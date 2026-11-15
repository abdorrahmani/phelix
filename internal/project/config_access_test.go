package project

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

func TestConfigAccessPreservesSectionPermissions(t *testing.T) {
	cases := map[string][2]string{
		"health":             {"health: {endpoints: [{name: ready, path: /ready}]}", "health: {endpoints: [{name: ready, path: /changed}]}"},
		"webhook":            {"webhook: {enabled: false}", "webhook: {enabled: true}"},
		"hooks":              {"hooks: {pre_build: {command: [./build], future: 1}}", "hooks: {pre_build: {command: [./build], future: 2}}"},
		"hooks.*.env":        {"hooks: {pre_build: {command: [./build], env: {PUBLIC: yes}}}", "hooks: {pre_build: {command: [./build], env: {PUBLIC: changed}}}"},
		"secrets":            {"secrets: {imports: [{secret: db, env: DATABASE_URL}]}", "secrets: {imports: [{secret: other, env: DATABASE_URL}]}"},
		"security":           {"security: {vulnerability: {enabled: false}}", "security: {vulnerability: {enabled: true}}"},
		"deploy.autoscaling": {"deploy: {strategy: rolling, autoscaling: {enabled: false}}", "deploy: {strategy: rolling, autoscaling: {enabled: true}}"},
		"job":                {"job: {command: [./job]}", "job: {command: [./other]}"},
		"schedule":           {"schedule: {cron: '@daily'}", "schedule: {cron: '@hourly'}"},
		"type.job":           {"type: service", "type: job"},
	}
	for path, pair := range cases {
		t.Run(path, func(t *testing.T) {
			denied := []ConfigDeniedPath{{Path: path, Message: "permission or subscription is required"}}
			before := []byte("name: old\n" + pair[0] + "\n")
			unchanged := []byte("# formatting and unrelated changes are allowed\nname: new\n" + pair[0] + "\n")
			if err := checkConfigWriteAccess(before, unchanged, denied); err != nil {
				t.Fatalf("unrelated edit denied: %v", err)
			}
			if err := checkConfigWriteAccess(before, []byte("name: old\n"+pair[1]+"\n"), denied); phelixerr.CodeOf(err) != phelixerr.CodePermissionDenied {
				t.Fatalf("protected change accepted: %v", err)
			}
		})
	}
	if err := checkConfigWriteAccess([]byte("type: service"), []byte("type: worker"), []ConfigDeniedPath{{Path: "type.job"}}); err != nil {
		t.Fatalf("worker gated as job: %v", err)
	}
}

func TestConfigAccessProtectsWholeFileReads(t *testing.T) {
	for path, raw := range map[string]string{
		"health": "health: {endpoints: []}", "security": "security: {vulnerability: {enabled: false}}",
		"secrets": "secrets: {imports: []}", "hooks.*.env": "hooks: {pre_build: {env: {TOKEN: private}}}",
	} {
		t.Run(path, func(t *testing.T) {
			denied := []ConfigDeniedPath{{Path: path, Message: "read permission is required"}}
			if err := CheckConfigReadAccess([]byte(raw), denied); err == nil {
				t.Fatal("protected section readable")
			}
			if err := CheckConfigReadAccess([]byte("name: public"), denied); err != nil {
				t.Fatal(err)
			}
			if err := CheckConfigReadAccess(nil, denied); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfigAccessResolvesAliasesWithoutRoundingUnknownNumbers(t *testing.T) {
	current := []byte("defaults: &defaults {env: {PUBLIC: exact}, future: 900719925474099312345678901234567890}\nhooks: {pre_build: {<<: *defaults, command: [./build]}}\n")
	same := []byte("hooks: {pre_build: {command: [./build], env: {PUBLIC: exact}, future: 900719925474099312345678901234567890}}\n")
	denied := []ConfigDeniedPath{{Path: "hooks", Message: "hook permission is required"}}
	if err := checkConfigWriteAccess(current, same, denied); err != nil {
		t.Fatalf("alias expansion changed permissions: %v", err)
	}
	changed := bytes.Replace(same, []byte("1234567890"), []byte("1234567891"), 1)
	if err := checkConfigWriteAccess(current, changed, denied); err == nil {
		t.Fatal("large unknown numeric change bypassed permission")
	}
	if err := CheckConfigReadAccess(current, []ConfigDeniedPath{{Path: "hooks.*.env", Message: "environment permission required"}}); err == nil {
		t.Fatal("merged environment bypassed read permission")
	}
	aliasBomb := "anchor: &a [one, two]\n"
	previous := "a"
	for _, name := range []string{"b", "c", "d", "e", "f", "g"} {
		aliasBomb += name + ": &" + name + " [" + strings.TrimSuffix(strings.Repeat("*"+previous+",", 10), ",") + "]\n"
		previous = name
	}
	if _, err := decodeConfigAccess([]byte(aliasBomb)); err == nil {
		t.Fatal("excessive alias expansion accepted")
	}
}

func TestConfigAccessEnforcesLockedSaveAndCreationDefaults(t *testing.T) {
	dir := t.TempDir()
	baseline, err := yaml.Marshal(InitialConfig("demo", TypeService, 8080))
	if err != nil {
		t.Fatal(err)
	}
	policy := ConfigAccessPolicy{WriteDeniedPaths: []ConfigDeniedPath{{Path: "health", Message: "health subscription is required"}}, InitialContent: baseline}
	initial, err := SaveContent(dir, baseline, SaveOptions{MustNotExist: true, AccessPolicy: policy})
	if err != nil {
		t.Fatalf("CLI initialization defaults were gated: %v", err)
	}
	changed := bytes.Replace(baseline, []byte("/health"), []byte("/ready"), 1)
	if _, err := SaveContent(dir, changed, SaveOptions{ExpectedRevision: initial.Revision, AccessPolicy: policy}); err == nil {
		t.Fatal("protected save accepted")
	}
	current, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || !bytes.Equal(current, baseline) {
		t.Fatalf("denied save changed file: %q %v", current, err)
	}
	stale := SaveOptions{ExpectedRevision: "old", AccessPolicy: ConfigAccessPolicy{ReadDeniedPaths: []ConfigDeniedPath{{Path: "health", Message: "health view permission required"}}}}
	saved, err := SaveContent(dir, changed, stale)
	if err == nil || errors.Is(err, ErrConfigConflict) || len(saved.Content) != 0 {
		t.Fatalf("conflict leaked protected source: %+v %v", saved, err)
	}
	deniedDir := t.TempDir()
	_, err = SaveContent(deniedDir, []byte("name: demo\nhooks: {pre_build: {command: [./build], env: {TOKEN: private}}}\n"), SaveOptions{MustNotExist: true, AccessPolicy: ConfigAccessPolicy{ReadDeniedPaths: []ConfigDeniedPath{{Path: "hooks.*.env", Message: "environment permission required"}}}})
	if err == nil {
		t.Fatal("creation returned unreadable configuration")
	}
	if _, err := os.Stat(filepath.Join(deniedDir, FileName)); !os.IsNotExist(err) {
		t.Fatal("denied creation wrote a file")
	}
}

func TestConfigAccessRejectsAmbiguousProtectedReads(t *testing.T) {
	for path, raw := range map[string]string{
		"hooks.*.env": "hooks: {pre_build: {env: {TOKEN: private}}}\nhooks: {}\n",
		"security":    "security: {vulnerability: {enabled: true}}\nsecurity: null\n",
		"health":      "health: {endpoints: [{name: private, path: /private}]}\nhealth: null\n",
		"secrets":     "secrets: {imports: [{secret: private, env: TOKEN}]}\nsecrets: null\n",
	} {
		t.Run(path, func(t *testing.T) {
			if err := CheckConfigReadAccess([]byte(raw), []ConfigDeniedPath{{Path: path, Message: "view permission required"}}); err == nil {
				t.Fatal("duplicate key exposed protected source")
			}
			if err := CheckConfigReadAccess([]byte(raw), nil); err != nil {
				t.Fatal("owner cannot repair invalid configuration")
			}
		})
	}
}
