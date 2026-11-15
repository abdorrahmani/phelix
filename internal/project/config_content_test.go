package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const validConfigContent = "name: demo\nport: 8080\nwatching: disable\n"

func TestReadContentMissingAndPresent(t *testing.T) {
	dir := t.TempDir()
	content, err := ReadContent(dir)
	if err != nil {
		t.Fatalf("ReadContent missing: %v", err)
	}
	if content.Exists {
		t.Fatal("missing file must report Exists=false")
	}

	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(validConfigContent), 0o644); err != nil {
		t.Fatal(err)
	}
	content, err = ReadContent(dir)
	if err != nil {
		t.Fatalf("ReadContent present: %v", err)
	}
	if !content.Exists || string(content.Content) != validConfigContent {
		t.Fatalf("content = %+v", content)
	}
	if content.Revision != ContentRevision([]byte(validConfigContent)) {
		t.Fatalf("revision = %q", content.Revision)
	}
}

func TestSaveContentCreatesAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	saved, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !saved.Changed || saved.Revision != ContentRevision([]byte(validConfigContent)) {
		t.Fatalf("saved = %+v", saved)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || string(raw) != validConfigContent {
		t.Fatalf("file = %q, err = %v", raw, err)
	}

	again, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil || again.Changed {
		t.Fatalf("idempotent create = %+v, err = %v", again, err)
	}

	_, err = SaveContent(dir, []byte("name: other\n"), SaveOptions{MustNotExist: true})
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("overwrite error = %v, want conflict", err)
	}
}

func TestSaveContentUpdateChecksRevision(t *testing.T) {
	dir := t.TempDir()
	first, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	next := "name: demo\nport: 9090\nwatching: disable\n"
	saved, err := SaveContent(dir, []byte(next), SaveOptions{ExpectedRevision: first.Revision})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !saved.Changed || saved.Revision != ContentRevision([]byte(next)) {
		t.Fatalf("saved = %+v", saved)
	}

	_, err = SaveContent(dir, []byte("name: stale\n"), SaveOptions{ExpectedRevision: first.Revision})
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale update error = %v, want conflict", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(raw) != next {
		t.Fatalf("conflict must not write; file = %q", raw)
	}
}

func TestSaveContentIdenticalContentIsNoOp(t *testing.T) {
	dir := t.TempDir()
	first, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	saved, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{ExpectedRevision: "stale"})
	if err != nil {
		t.Fatalf("idempotent update: %v", err)
	}
	if saved.Changed || saved.Revision != first.Revision {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestSaveContentUpdateRequiresExistingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{ExpectedRevision: "rev"})
	if err == nil {
		t.Fatal("update without a file must fail")
	}
}

func TestValidateContentRejectsInvalidConfiguration(t *testing.T) {
	dir := t.TempDir()
	if _, err := ValidateContent(dir, []byte("name: demo\nport: 70000\n")); err == nil {
		t.Fatal("invalid port must fail validation")
	}
	if _, err := ValidateContent(dir, []byte(validConfigContent)); err != nil {
		t.Fatalf("valid content: %v", err)
	}
}

func TestSaveContentRejectsOversizedDocument(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, MaxConfigBytes+1)
	for i := range raw {
		raw[i] = ' '
	}
	if _, err := SaveContent(dir, raw, SaveOptions{MustNotExist: true}); err == nil {
		t.Fatal("oversized content must fail")
	}
}

func TestRemoteValidationPreservesCLIConfigSemantics(t *testing.T) {
	dir := t.TempDir()
	cases := []string{
		"", "# empty configuration\n", "port: 0\n", "name: demo\nport: 65535\n",
		"type: worker\nport: 0\nhealth: {mode: process}\ndeploy: {strategy: classic, replicas: 1}\n",
		"type: job\nport: 0\nschedule: {cron: '@daily'}\njob: {command: [./job]}\n",
		"name: demo\nunknown: {nested: [1, true, null]}\n",
		"name: demo\nhealth: {endpoints: [{name: ready, path: /ready, mode: http}]}\n",
		"deploy: {strategy: progressive, rollout: {steps: [{traffic: 25, duration: 1s}, {traffic: 100}]}}\n",
		"port: 70000\n", "watching: yes\n", "deploy: {strategy: bogus}\n",
		"health: {endpoints: [{name: ready, path: /a}, {name: ready, path: /b}]}\n",
		"deploy: {strategy: progressive, rollout: {steps: [{traffic: 50}, {traffic: 25}]}}\n",
	}
	for _, raw := range cases {
		t.Run(fmt.Sprintf("%q", raw), func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, localErr := Load(dir)
			_, remoteErr := ValidateContent(dir, []byte(raw))
			if (localErr == nil) != (remoteErr == nil) {
				t.Fatalf("local = %v, remote = %v", localErr, remoteErr)
			}
		})
	}
}

func TestRemoteValidationRejectsUnsafeDocuments(t *testing.T) {
	for name, raw := range map[string]string{
		"syntax": "name: [oops", "duplicate": "unknown: {key: 1, key: 2}",
		"documents": "name: first\n---\nname: second\n", "tag": "unknown: !exec command\n",
		"depth": "unknown: " + strings.Repeat("[", 110) + "1" + strings.Repeat("]", 110),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateContent(t.TempDir(), []byte(raw)); err == nil {
				t.Fatal("unsafe document accepted")
			}
		})
	}
}

func TestRemoteConfigurationPreservesReportingConsent(t *testing.T) {
	for _, current := range []string{
		"name: demo\nsecurity: {vulnerability: {reporting: false}}\n",
		"name: demo\nsecurity: {vulnerability: {reporting: true}}\n",
		"port: broken\nsecurity: {vulnerability: {reporting: true}}\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(current), 0o600); err != nil {
			t.Fatal(err)
		}
		previous := strings.Contains(current, "reporting: true")
		for _, value := range []bool{false, true} {
			raw := []byte(fmt.Sprintf("name: demo\nport: 8080\nsecurity: {vulnerability: {reporting: %t}}\n", value))
			_, err := ValidateRemoteContent(dir, raw, ConfigAccessPolicy{})
			if (err == nil) != (value == previous) {
				t.Fatalf("value %t previous %t validation: %v", value, previous, err)
			}
			_, err = SaveContent(dir, raw, SaveOptions{ExpectedRevision: ContentRevision([]byte(current))})
			if value != previous && err == nil {
				t.Fatal("remote reporting consent changed")
			}
			if err == nil {
				current = string(raw)
			}
		}
	}
}

func TestConfigFileAndLockRejectSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege depends on host settings")
	}
	for _, name := range []string{FileName, ".phelix-config.lock"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(target, []byte("private content"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if name == FileName {
				if _, err := ReadContent(dir); err == nil {
					t.Fatal("symlink read accepted")
				}
			}
			if _, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true}); err == nil {
				t.Fatal("symlink save accepted")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "private content" {
				t.Fatalf("private target changed: %q %v", got, err)
			}
		})
	}
}

func TestConfigAtomicPermissionsAndInvalidSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(validConfigContent), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := ReadContent(dir)
	if _, err := SaveContent(dir, []byte("port: 90000\n"), SaveOptions{ExpectedRevision: before.Revision}); err == nil {
		t.Fatal("invalid save accepted")
	}
	after, _ := ReadContent(dir)
	if after.Revision != before.Revision {
		t.Fatal("invalid save modified file")
	}
	if _, err := SaveContent(dir, []byte("name: updated\n"), SaveOptions{ExpectedRevision: before.Revision}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions changed: %o", info.Mode().Perm())
	}
	entries, _ := filepath.Glob(filepath.Join(dir, ".phelix-config-*"))
	if len(entries) != 0 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestConfigSavePermissionFailurePreservesFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires non-root Unix permissions")
	}
	dir := t.TempDir()
	initial, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	_, err = SaveContent(dir, []byte("name: changed\n"), SaveOptions{ExpectedRevision: initial.Revision})
	if err == nil {
		t.Fatal("unwritable directory accepted")
	}
	after, err := ReadContent(dir)
	if err != nil || after.Revision != initial.Revision {
		t.Fatalf("file changed: %v %v", after, err)
	}
}

func TestConfigConcurrentSaveAndAtomicReads(t *testing.T) {
	dir := t.TempDir()
	first, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatal(err)
	}
	const writers = 12
	var wg sync.WaitGroup
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := SaveContent(dir, []byte(fmt.Sprintf("name: demo%d\n", index)), SaveOptions{ExpectedRevision: first.Revision})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConfigConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful conflicting saves: %d", success)
	}
	current, err := ReadContent(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte("name: demo\nunknown: |\n  "), bytes.Repeat([]byte("x"), MaxConfigBytes/2)...)
	stop := make(chan struct{})
	failures := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			content, err := ReadContent(dir)
			if err == nil {
				_, err = ValidateContent(dir, content.Content)
			}
			if err != nil {
				select {
				case failures <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 5; i++ {
		next := raw
		if i%2 == 1 {
			next = []byte(validConfigContent)
		}
		saved, err := SaveContent(dir, next, SaveOptions{ExpectedRevision: current.Revision})
		if err != nil {
			t.Fatal(err)
		}
		current = saved.ConfigContent
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatalf("atomic read: %v", err)
	default:
	}
}

func TestConfigLockProcessHelper(t *testing.T) {
	dir := os.Getenv("PHELIX_CONFIG_LOCK_TEST_DIR")
	if dir == "" {
		return
	}
	_, err := SaveContent(dir, []byte(os.Getenv("PHELIX_CONFIG_LOCK_CONTENT")), SaveOptions{ExpectedRevision: os.Getenv("PHELIX_CONFIG_LOCK_REVISION")})
	outcome := "success"
	if errors.Is(err, ErrConfigConflict) {
		outcome = "conflict"
	} else if err != nil {
		t.Fatal(err)
	}
	fmt.Println(outcome)
}

func TestConfigCrossProcessLock(t *testing.T) {
	dir := t.TempDir()
	initial, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outputs := make(chan string, 2)
	for _, raw := range []string{"name: process1\n", "name: process2\n"} {
		command := exec.Command(os.Args[0], "-test.run=^TestConfigLockProcessHelper$")
		command.Env = append(os.Environ(), "PHELIX_CONFIG_LOCK_TEST_DIR="+dir, "PHELIX_CONFIG_LOCK_REVISION="+initial.Revision, "PHELIX_CONFIG_LOCK_CONTENT="+raw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := command.CombinedOutput()
			if err != nil {
				outputs <- "error: " + string(out)
			} else {
				outputs <- string(out)
			}
		}()
	}
	wg.Wait()
	close(outputs)
	successes, conflicts := 0, 0
	for out := range outputs {
		if strings.HasPrefix(out, "success") {
			successes++
		} else if strings.HasPrefix(out, "conflict") {
			conflicts++
		} else {
			t.Fatal(out)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("process saves success=%d conflict=%d", successes, conflicts)
	}
}

func TestEditorSchemaIncludesAllMapFieldsAndSentinels(t *testing.T) {
	raw, err := EditorSchemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	fields := schema["properties"].(map[string]any)
	if fields["port"].(map[string]any)["minimum"] != float64(0) {
		t.Fatal("worker port zero rejected")
	}
	hooks := fields["hooks"].(map[string]any)["properties"].(map[string]any)
	for _, name := range HookNames {
		env := hooks[name].(map[string]any)["properties"].(map[string]any)["env"].(map[string]any)
		if env["type"] != "object" || env["additionalProperties"].(map[string]any)["type"] != "string" {
			t.Fatalf("%s env schema: %v", name, env)
		}
	}
	security := fields["security"].(map[string]any)["properties"].(map[string]any)["vulnerability"].(map[string]any)["properties"].(map[string]any)
	if security["reporting"].(map[string]any)["readOnly"] != true {
		t.Fatal("local reporting consent is editable")
	}
	if len(security["fail_threshold"].(map[string]any)["enum"].([]any)) != 4 {
		t.Fatal("missing severity options")
	}
}

func TestInitialConfigMatchesInitializationDefaults(t *testing.T) {
	for _, kind := range []string{TypeService, TypeWorker} {
		cfg := InitialConfig("demo", kind, 8080)
		if err := validateConfig(t.TempDir(), cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Watching != WatchingDisable {
			t.Fatal("initial configuration enables monitoring")
		}
		if kind == TypeWorker && cfg.Port != 0 {
			t.Fatal("worker has a public port")
		}
		if kind == TypeService && cfg.Health.Endpoints[0].Path != "/health" {
			t.Fatal("initial health endpoint changed")
		}
	}
}

func TestRemoteReportingConsentSupportsAliasesAndMergeKeys(t *testing.T) {
	for name, raw := range map[string]string{
		"alias":       "defaults: &defaults {vulnerability: {reporting: true}}\nsecurity: *defaults\nport: broken\n",
		"merged":      "defaults: &defaults {reporting: true}\nsecurity: {vulnerability: {<<: *defaults}}\nport: broken\n",
		"root merged": "defaults: &defaults {security: {vulnerability: {reporting: true}}}\n<<: *defaults\nport: broken\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			preserved := []byte("name: repaired\nport: 8080\nsecurity: {vulnerability: {reporting: true}}\n")
			if _, err := SaveContent(dir, preserved, SaveOptions{ExpectedRevision: ContentRevision([]byte(raw))}); err != nil {
				t.Fatalf("preserving alias consent: %v", err)
			}
			disabled := []byte("name: repaired\nport: 8080\nsecurity: {vulnerability: {reporting: false}}\n")
			if _, err := SaveContent(dir, disabled, SaveOptions{ExpectedRevision: ContentRevision(preserved)}); err == nil {
				t.Fatal("disabling aliased local consent accepted")
			}
		})
	}
}

func TestConfigAtomicReplacementDetectsExternalEdit(t *testing.T) {
	dir := t.TempDir()
	initial, err := SaveContent(dir, []byte(validConfigContent), SaveOptions{MustNotExist: true})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, FileName)
	edited := []byte("name: externally-edited\n")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigAtomic(path, []byte("name: dashboard\n"), initial.ConfigContent); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("external edit was not detected: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, edited) {
		t.Fatalf("external edit overwritten: %q %v", after, err)
	}
	current, err := ReadContent(dir)
	if err != nil {
		t.Fatal(err)
	}
	edited = []byte("name: second-external-edit\n")
	done := make(chan error, 1)
	raw := append([]byte("name: dashboard\nunknown: "), bytes.Repeat([]byte("x"), 16<<20)...)
	go func() { done <- writeConfigAtomic(path, raw, current) }()
	changed := false
	timeout := time.After(5 * time.Second)
	for !changed {
		entries, _ := filepath.Glob(filepath.Join(dir, ".phelix-config-*"))
		if len(entries) > 0 {
			if err := os.WriteFile(path, edited, 0o600); err != nil {
				t.Fatal(err)
			}
			changed = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("replacement completed before external edit: %v", err)
		case <-timeout:
			t.Fatal("temporary configuration was not created")
		default:
			runtime.Gosched()
		}
	}
	if err := <-done; !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("external edit during replacement not detected: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(after, edited) {
		t.Fatalf("external edit overwritten during replacement: %q %v", after, err)
	}
}

func TestInitialJobConfigurationRequiresRealCommand(t *testing.T) {
	cfg := InitialConfig("scheduled-job", TypeJob, 8080)
	if cfg.Type != TypeJob || cfg.Port != 0 || cfg.Deploy != nil || cfg.Health != nil || cfg.Schedule == nil || cfg.Job == nil {
		t.Fatalf("job identity lost: %+v", cfg)
	}
	if err := validateConfig(t.TempDir(), cfg); err == nil {
		t.Fatal("empty job command accepted")
	}
	cfg.Job.Command = []string{"./scheduled-job"}
	if err := validateConfig(t.TempDir(), cfg); err != nil {
		t.Fatal(err)
	}
}
