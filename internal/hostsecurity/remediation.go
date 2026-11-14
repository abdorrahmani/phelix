package hostsecurity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/google/uuid"
)

// Remediation bounds and vocabulary.
const (
	// PreviewTTL bounds how long a preview token stays confirmable.
	PreviewTTL = 30 * time.Minute
	// PreviewTimeout bounds one preview dry-run.
	PreviewTimeout = 5 * time.Minute
	// ApplyTimeout bounds one apply execution.
	ApplyTimeout = 15 * time.Minute
	// operationsFileName persists preview tokens and update operations
	// durably so they survive restarts.
	operationsFileName = "operations.json"
	// operationsVersion is the on-disk schema of operations.json.
	operationsVersion = 1
	// maxStoredOperations bounds the operation history; the oldest
	// terminal operations are pruned first.
	maxStoredOperations = 100
)

// Operation lifecycle statuses.
const (
	OperationStatusExecuting = "executing"
	OperationStatusSucceeded = "succeeded"
	OperationStatusFailed    = "failed"
	OperationStatusVerified  = "verified"
)

// UpdateItem is one package's planned update path.
type UpdateItem struct {
	Name              string   `json:"name"`
	CurrentVersion    string   `json:"current_version"`
	TargetVersion     string   `json:"target_version"`
	DependencyChanges []string `json:"dependency_changes,omitempty"`
	Removals          []string `json:"removals,omitempty"`
}

// Preview is the dry-run plan a later apply confirms through its token.
type Preview struct {
	Token          string       `json:"preview_token"`
	ExpiresAt      time.Time    `json:"expires_at"`
	Manager        string       `json:"manager"`
	Items          []UpdateItem `json:"items"`
	Warnings       []string     `json:"warnings,omitempty"`
	Limitations    []string     `json:"limitations,omitempty"`
	RebootRequired bool         `json:"reboot_required"`
	CreatedAt      time.Time    `json:"created_at"`
}

// VerificationEntry is one requested package's post-apply outcome.
type VerificationEntry struct {
	Package  string `json:"package"`
	Resolved bool   `json:"resolved"`
	Detail   string `json:"detail"`
}

// UpdateResult is the terminal record of one apply operation.
type UpdateResult struct {
	OperationID    string              `json:"operation_id"`
	Status         string              `json:"status"`
	Manager        string              `json:"manager"`
	Summary        string              `json:"summary"`
	Error          string              `json:"error,omitempty"`
	Verification   []VerificationEntry `json:"verification,omitempty"`
	RebootRequired bool                `json:"reboot_required"`
	StartedAt      time.Time           `json:"started_at"`
	FinishedAt     time.Time           `json:"finished_at"`
}

// storedPreview is the durable binding between a preview token, the state
// it captured, and its expiry.
type storedPreview struct {
	Token     string       `json:"token"`
	Manager   string       `json:"manager"`
	Packages  []string     `json:"packages"`
	StateHash string       `json:"state_hash"`
	ExpiresAt time.Time    `json:"expires_at"`
	CreatedAt time.Time    `json:"created_at"`
	Items     []UpdateItem `json:"items"`
}

// storedOperation is one durable update operation record.
type storedOperation struct {
	OperationID  string        `json:"operation_id"`
	Type         string        `json:"type"`
	Status       string        `json:"status"`
	Packages     []string      `json:"packages"`
	PreviewToken string        `json:"preview_token,omitempty"`
	Manager      string        `json:"manager,omitempty"`
	Result       *UpdateResult `json:"result,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// operationsFile is the durable operations.json document.
type operationsFile struct {
	Version    int               `json:"version"`
	Previews   []storedPreview   `json:"previews"`
	Operations []storedOperation `json:"operations"`
}

// operationsLockPath is the flock guard around operations.json.
func operationsLockPath(dataDir string) string {
	return filepath.Join(dataDir, "host-security", "operations.lock")
}

// operationsApplyLockPath is the flock held while one update operation
// executes. The kernel releases it when the process dies, so a free lock
// proves that no live process is executing the recorded operation.
func operationsApplyLockPath(dataDir string) string {
	return filepath.Join(dataDir, "host-security", "apply.lock")
}

// acquireApplyLock takes the apply execution lock without blocking, creating
// its directory when needed.
func acquireApplyLock(dataDir string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(operationsApplyLockPath(dataDir)), 0700); err != nil {
		return nil, err
	}
	return acquireHostLock(operationsApplyLockPath(dataDir), false)
}

// abandonedOperationMessage is the indeterminate marker persisted on a
// reclaimed operation.
const abandonedOperationMessage = "the agent terminated while the update was executing; its outcome is indeterminate"

// operationsPath is the durable preview and operation store.
func operationsPath(dataDir string) string {
	return filepath.Join(dataDir, "host-security", operationsFileName)
}

// reclaimAbandonedOperations transitions executing operations to failed with
// an indeterminate marker when the apply lock is free, mirroring the
// scheduler's abandoned-scan recovery. A held lock means a live process owns
// the executing records. It reports whether any record changed.
func reclaimAbandonedOperations(data *operationsFile, dataDir string) bool {
	executing := false
	for i := range data.Operations {
		if data.Operations[i].Status == OperationStatusExecuting {
			executing = true
			break
		}
	}
	if !executing {
		return false
	}
	lock, err := acquireApplyLock(dataDir)
	if err != nil {
		return false
	}
	defer func() {
		_ = releaseHostLock(lock)
		_ = lock.Close()
	}()
	now := time.Now().UTC()
	changed := false
	for i := range data.Operations {
		op := &data.Operations[i]
		if op.Status != OperationStatusExecuting {
			continue
		}
		op.Status = OperationStatusFailed
		op.UpdatedAt = now
		if op.Result == nil {
			op.Result = &UpdateResult{}
		}
		op.Result.OperationID = op.OperationID
		op.Result.Status = OperationStatusFailed
		op.Result.Manager = op.Manager
		op.Result.Error = abandonedOperationMessage
		if op.Result.StartedAt.IsZero() {
			op.Result.StartedAt = op.CreatedAt
		}
		op.Result.FinishedAt = now
		changed = true
	}
	return changed
}

// loadOperationsFile reads and validates the operations document without
// taking any lock. Callers that only read must use loadOperations, and
// callers that mutate must use mutateOperations, so the whole transaction
// stays inside the operations lock.
func loadOperationsFile(dataDir string) (operationsFile, error) {
	data := operationsFile{Version: operationsVersion}
	raw, err := os.ReadFile(operationsPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return data, nil
	}
	if err != nil {
		return data, err
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return operationsFile{}, errors.New("host security operations store is malformed")
	}
	if data.Version != operationsVersion {
		return operationsFile{}, fmt.Errorf("unsupported host security operations store version %d", data.Version)
	}
	return data, nil
}

// saveOperationsFile atomically persists the operations document without
// taking any lock; the caller must already hold the operations lock.
func saveOperationsFile(dataDir string, data operationsFile) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(operationsPath(dataDir)), "operations-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if err = os.Chmod(tmpName, 0o600); err == nil {
			err = os.Rename(tmpName, operationsPath(dataDir))
		}
	}
	if err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// operationsGuard holds the operations lock for one read-modify-write
// transaction over the durable operations store.
type operationsGuard struct {
	lock *os.File
}

func acquireOperationsGuard(dataDir string) (*operationsGuard, error) {
	if err := os.MkdirAll(filepath.Dir(operationsPath(dataDir)), 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireHostLock(operationsLockPath(dataDir), true)
	if err != nil {
		return nil, err
	}
	return &operationsGuard{lock: lock}, nil
}

func (g *operationsGuard) release() {
	_ = releaseHostLock(g.lock)
	_ = g.lock.Close()
}

// loadOperations reads the operations document; a missing file is an empty
// store. Executing operations abandoned by a dead process are reclaimed on
// every load. The reclaim and the read run under the operations lock so a
// concurrent mutation cannot be lost between them.
func loadOperations(dataDir string) (operationsFile, error) {
	guard, err := acquireOperationsGuard(dataDir)
	if err != nil {
		return operationsFile{}, err
	}
	defer guard.release()
	data, err := loadOperationsFile(dataDir)
	if err != nil {
		return operationsFile{}, err
	}
	if reclaimAbandonedOperations(&data, dataDir) {
		if err := saveOperationsFile(dataDir, data); err != nil {
			return operationsFile{}, err
		}
	}
	return data, nil
}

// saveOperations atomically persists the operations document under an
// exclusive flock so concurrent daemon processes cannot interleave writes.
func saveOperations(dataDir string, data operationsFile) error {
	guard, err := acquireOperationsGuard(dataDir)
	if err != nil {
		return err
	}
	defer guard.release()
	return saveOperationsFile(dataDir, data)
}

// mutateOperations runs one read-modify-write transaction over the durable
// operations store. The operations lock is held across the entire
// load->mutate->save critical section, so concurrent writers can never lose
// each other's updates. The abandoned-operation reclaim runs inside the
// same critical section; its apply-lock probe is non-blocking, so it can
// never deadlock against a live apply holding that lock.
func mutateOperations(dataDir string, mutate func(*operationsFile) error) error {
	guard, err := acquireOperationsGuard(dataDir)
	if err != nil {
		return err
	}
	defer guard.release()
	data, err := loadOperationsFile(dataDir)
	if err != nil {
		return err
	}
	if reclaimAbandonedOperations(&data, dataDir) {
		if err := saveOperationsFile(dataDir, data); err != nil {
			return err
		}
	}
	if err := mutate(&data); err != nil {
		return err
	}
	return saveOperationsFile(dataDir, data)
}

// pruneLocked drops expired previews and the oldest terminal operations
// beyond the retention cap.
func pruneLocked(data *operationsFile, now time.Time) {
	previews := data.Previews[:0]
	for _, preview := range data.Previews {
		if preview.ExpiresAt.After(now) {
			previews = append(previews, preview)
		}
	}
	data.Previews = previews
	active := 0
	for _, op := range data.Operations {
		if op.Status == OperationStatusExecuting {
			active++
		}
	}
	terminal := 0
	for _, op := range data.Operations {
		if op.Status != OperationStatusExecuting {
			terminal++
		}
	}
	for active+terminal > maxStoredOperations && terminal > 0 {
		for i, op := range data.Operations {
			if op.Status != OperationStatusExecuting {
				data.Operations = append(data.Operations[:i], data.Operations[i+1:]...)
				terminal--
				break
			}
		}
	}
}

// newPreviewToken returns a random 32 hex character token.
func newPreviewToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// stateHashOf binds a preview to the target packages' versions captured at
// preview time: a sorted name@version list, hashed.
func stateHashOf(entries [][2]string) string {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i][0] != entries[j][0] {
			return entries[i][0] < entries[j][0]
		}
		return entries[i][1] < entries[j][1]
	})
	var builder strings.Builder
	for _, entry := range entries {
		builder.WriteString(entry[0])
		builder.WriteString("@")
		builder.WriteString(entry[1])
		builder.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// SupportedRemediationManager reports whether the engine can drive the
// host's package manager.
func SupportedRemediationManager(manager string) bool {
	switch manager {
	case "apt", "dnf", "zypper", "apk", "pacman":
		return true
	}
	return false
}

// validatePackageList enforces the wire-level list bounds for one update
// operation.
func validatePackageList(packages []string) error {
	if len(packages) == 0 {
		return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "at least one package is required")
	}
	if len(packages) > MaxUpdatePackages {
		return phelixerr.Newf(phelixerr.CodeHostSecurityInvalid, "at most %d packages per operation, got %d", MaxUpdatePackages, len(packages))
	}
	seen := map[string]bool{}
	for _, spec := range packages {
		if err := ValidatePackageSpec(spec); err != nil {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, err.Error())
		}
		name, _ := SplitPackageSpec(spec)
		if seen[name] {
			return phelixerr.Newf(phelixerr.CodeHostSecurityInvalid, "package %s is listed more than once", name)
		}
		seen[name] = true
	}
	return nil
}

// packageNames strips any pinned versions, preserving the request order.
func packageNames(packages []string) []string {
	out := make([]string, 0, len(packages))
	for _, spec := range packages {
		name, _ := SplitPackageSpec(spec)
		out = append(out, name)
	}
	return out
}

// PrivilegeProbe reports the effective uid for the privilege gate. Tests
// replace it to exercise both sides of the gate.
var PrivilegeProbe = os.Geteuid

// requirePrivilege fails the operation before anything executes when the
// process cannot mutate the package database. There is no auto-sudo.
func requirePrivilege() error {
	if PrivilegeProbe() == 0 {
		return nil
	}
	return phelixerr.New(phelixerr.CodeHostSecurityPrivilege,
		"package updates require root privileges; run the operation as root (no automatic privilege escalation)")
}

// PreviewUpdates evaluates the update path for the requested packages with
// the manager's native dry-run. Read-only: no transaction is executed and
// no package manager lock is taken or released.
func PreviewUpdates(ctx context.Context, system System, host Host, packages []string, dataDir string) (Preview, error) {
	out := Preview{Manager: host.Manager, Warnings: []string{}, Limitations: []string{}, Items: []UpdateItem{}, CreatedAt: time.Now().UTC()}
	if err := validatePackageList(packages); err != nil {
		return out, err
	}
	if !SupportedRemediationManager(host.Manager) {
		return out, phelixerr.Newf(phelixerr.CodeHostSecurityUnsupported,
			"package manager %q has no update preview adapter", host.Manager)
	}
	ctx, cancel := context.WithTimeout(ctx, PreviewTimeout)
	defer cancel()

	versions, err := currentVersions(ctx, system, host.Manager, packageNames(packages))
	if err != nil {
		return out, err
	}
	targets := []string{}
	for _, name := range packageNames(packages) {
		version, ok := versions[name]
		if !ok || version == "" {
			out.Warnings = append(out.Warnings, "package "+name+" is not installed; it is not part of this preview")
			continue
		}
		targets = append(targets, name)
	}
	if len(targets) == 0 {
		return out, phelixerr.New(phelixerr.CodeHostSecurityInvalid, "none of the requested packages is installed")
	}

	name, args := previewCommand(host.Manager, targets)
	raw, err := system.Runner.Run(ctx, name, args...)
	if err != nil {
		return out, phelixerr.Wrap(phelixerr.CodeHostSecurityExecFailed, "the package manager dry-run failed", err)
	}
	items, warnings := parsePreview(host.Manager, targets, versions, raw)
	out.Items = items
	out.Warnings = append(out.Warnings, warnings...)
	out.Limitations = previewLimitations(host.Manager)
	entries := make([][2]string, 0, len(targets))
	for _, item := range items {
		entries = append(entries, [2]string{item.Name, item.CurrentVersion})
	}
	out.Token, err = newPreviewToken()
	if err != nil {
		return out, err
	}
	out.ExpiresAt = out.CreatedAt.Add(PreviewTTL)
	if state, known := DetectReboot(ctx, system, host); known {
		out.RebootRequired = state.Required
	}
	if err := mutateOperations(dataDir, func(data *operationsFile) error {
		pruneLocked(data, out.CreatedAt)
		data.Previews = append(data.Previews, storedPreview{
			Token: out.Token, Manager: host.Manager, Packages: targets, StateHash: stateHashOf(entries),
			ExpiresAt: out.ExpiresAt, CreatedAt: out.CreatedAt, Items: out.Items,
		})
		return nil
	}); err != nil {
		return out, err
	}
	return out, nil
}

// previewCommand is the manager's native dry-run invocation.
func previewCommand(manager string, packages []string) (string, []string) {
	switch manager {
	case "apt":
		return "apt-get", append([]string{"-s", "install"}, packages...)
	case "dnf":
		return "dnf", append([]string{"-q", "--assumeno", "upgrade"}, packages...)
	case "zypper":
		return "zypper", append([]string{"-q", "--non-interactive", "update", "--dry-run"}, packages...)
	case "apk":
		return "apk", append([]string{"-s", "upgrade"}, packages...)
	case "pacman":
		return "pacman", append([]string{"--print-format", "%n %v", "-Su"}, packages...)
	}
	return "", nil
}

// previewLimitations is the manager's mandatory disclosure text.
func previewLimitations(manager string) []string {
	switch manager {
	case "apt":
		return []string{"apt simulation is a resolver dry-run, not a transaction plan from the resolver lock"}
	case "pacman":
		return []string{"pacman cannot simulate dependency conflicts or package removals, and cannot install a pinned older version; apply installs latest available"}
	}
	return nil
}

var (
	aptInstLine = regexp.MustCompile(`^Inst (\S+) \[(\S+)\] \(([^)\s]+)`)
	aptConfLine = regexp.MustCompile(`^Conf (\S+) \(([^)\s]+)`)
	aptRemvLine = regexp.MustCompile(`^Remv (\S+) \[(\S+)\]`)
	apkUpdate   = regexp.MustCompile(`Upgrading (\S+) \((\S+) -> (\S+)\)`)
)

// parsePreview converts one dry-run's output into update items. The
// current versions come from the uniform installed-version query, never
// from the simulation text.
func parsePreview(manager string, packages []string, versions map[string]string, raw []byte) ([]UpdateItem, []string) {
	warnings := []string{}
	out := make([]UpdateItem, 0, len(packages))
	targets := map[string]string{}
	dependencyChanges := map[string][]string{}
	removals := map[string][]string{}
	lastTarget := ""
	switch manager {
	case "apt":
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimRight(line, "\r")
			if match := aptInstLine.FindStringSubmatch(line); match != nil {
				targets[match[1]] = match[3]
				lastTarget = match[1]
				continue
			}
			if match := aptConfLine.FindStringSubmatch(line); match != nil {
				entry := "configure " + match[1] + " (" + match[2] + ")"
				if _, ok := targets[match[1]]; ok {
					dependencyChanges[match[1]] = append(dependencyChanges[match[1]], entry)
				} else if lastTarget != "" {
					dependencyChanges[lastTarget] = append(dependencyChanges[lastTarget], entry)
				}
				continue
			}
			if match := aptRemvLine.FindStringSubmatch(line); match != nil {
				entry := "remove " + match[1] + " (" + match[2] + ")"
				if lastTarget != "" {
					removals[lastTarget] = append(removals[lastTarget], entry)
				}
			}
		}
	case "dnf":
		section := false
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
			switch {
			case trimmed == "" || strings.HasPrefix(trimmed, "Transaction Summary") || strings.HasPrefix(trimmed, "Installing:"):
				section = false
				continue
			case strings.HasPrefix(trimmed, "Upgrading"), strings.HasPrefix(trimmed, "Upgrading Software Repositories"):
				section = !strings.HasPrefix(trimmed, "Upgrading Software Repositories")
				continue
			}
			if !section {
				continue
			}
			name, target, ok := parseDNFUpgradeLine(trimmed)
			if ok {
				targets[name] = target
			}
		}
	case "zypper":
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
			tokens := strings.Fields(trimmed)
			if len(tokens) == 4 && tokens[2] == "->" {
				targets[tokens[0]] = tokens[3]
			}
		}
	case "apk":
		for _, match := range apkUpdate.FindAllStringSubmatch(string(raw), -1) {
			targets[match[1]] = match[3]
		}
	case "pacman":
		for _, line := range strings.Split(string(raw), "\n") {
			tokens := strings.Fields(strings.TrimRight(line, "\r"))
			if len(tokens) == 2 {
				targets[tokens[0]] = tokens[1]
			}
		}
	}
	for _, name := range packages {
		current := versions[name]
		target := current
		if parsed, ok := targets[name]; ok {
			target = parsed
		}
		item := UpdateItem{Name: name, CurrentVersion: current, TargetVersion: target}
		item.DependencyChanges = dependencyChanges[name]
		item.Removals = removals[name]
		if target == current {
			warnings = append(warnings, "package "+name+" has no update candidate; it stays at "+current)
		}
		if item.DependencyChanges == nil {
			item.DependencyChanges = []string{}
		}
		if item.Removals == nil {
			item.Removals = []string{}
		}
		out = append(out, item)
	}
	return out, warnings
}

// parseDNFUpgradeLine handles both the dnf4 table (target version only)
// and the dnf5 arrow form (old -> new).
func parseDNFUpgradeLine(line string) (string, string, bool) {
	tokens := strings.Fields(line)
	if len(tokens) < 3 {
		return "", "", false
	}
	name := tokens[0]
	if idx := strings.Index(name, "."); idx > 0 && !strings.Contains(name[idx:], "=") {
		name = name[:idx]
	}
	for i, token := range tokens {
		if token == "->" && i+1 < len(tokens) {
			return name, tokens[i+1], true
		}
	}
	return name, tokens[2], true
}

// currentVersionCommand is the uniform installed-version query per
// manager.
func currentVersionCommand(manager string, packages []string) (string, []string) {
	switch manager {
	case "apt":
		return "dpkg-query", append([]string{"-W", "-f=${Package}\t${db:Status-Abbrev}\t${Version}\n"}, packages...)
	case "dnf", "zypper":
		return "rpm", append([]string{"-q", "--qf", "%{NAME}\t%{EPOCHNUM}:%{VERSION}-%{RELEASE}\n"}, packages...)
	case "apk":
		return "apk", append([]string{"info", "-v"}, packages...)
	case "pacman":
		return "pacman", append([]string{"-Qi"}, packages...)
	}
	return "", nil
}

// currentVersions queries the installed version of every target package.
// Packages the manager does not report are absent from the result.
func currentVersions(ctx context.Context, system System, manager string, packages []string) (map[string]string, error) {
	name, args := currentVersionCommand(manager, packages)
	if name == "" {
		return nil, phelixerr.Newf(phelixerr.CodeHostSecurityUnsupported, "package manager %q has no inventory adapter", manager)
	}
	raw, err := system.Runner.Run(ctx, name, args...)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeHostSecurityExecFailed, "the installed version query failed", err)
	}
	out := map[string]string{}
	switch manager {
	case "apt":
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
			if len(fields) >= 3 && strings.HasPrefix(fields[1], "i") {
				out[fields[0]] = fields[2]
			}
		}
	case "dnf", "zypper":
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
			if len(fields) >= 2 {
				out[fields[0]] = fields[1]
			}
		}
	case "apk":
		for _, line := range strings.Split(string(raw), "\n") {
			entry := strings.TrimRight(line, "\r")
			for _, prefix := range packages {
				if strings.HasPrefix(entry, prefix+"-") {
					out[prefix] = strings.TrimPrefix(entry, prefix+"-")
				}
			}
		}
	case "pacman":
		currentName := ""
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
			if strings.HasPrefix(trimmed, "Name") {
				if _, value, ok := strings.Cut(trimmed, ":"); ok {
					currentName = strings.TrimSpace(value)
				}
				continue
			}
			if strings.HasPrefix(trimmed, "Version") && currentName != "" {
				if _, value, ok := strings.Cut(trimmed, ":"); ok {
					out[currentName] = strings.TrimSpace(value)
				}
				currentName = ""
			}
		}
	}
	return out, nil
}

// managerLockPaths lists the lock files whose flock evidence marks a busy
// package database. Locks are never removed and never waited out.
func managerLockPaths(manager string) []string {
	switch manager {
	case "apt":
		return []string{"/var/lib/dpkg/lock-frontend", "/var/lib/dpkg/lock", "/var/lib/apt/lists/lock"}
	case "dnf", "zypper":
		return []string{"/var/lib/rpm/.rpmlock"}
	case "apk":
		return []string{"/lib/apk/db/lock"}
	case "pacman":
		return []string{"/var/lib/pacman/db.lck"}
	}
	return nil
}

// managerLockPatterns are the output fragments the managers print when a
// lock is held elsewhere.
var managerLockPatterns = []string{
	"could not get lock",
	"unable to acquire the dpkg frontend lock",
	"another app is currently holding",
	"waiting for process with pid",
	"system management is locked by",
	"another instance is running",
	"unable to lock database",
	"could not lock database",
	"failed to lock database",
}

// lockHeld probes the manager's lock files without blocking and without
// ever creating one: the probe opens read-only, so a missing lock file
// stays missing. A missing or unreadable lock file cannot prove a busy
// package database. The probe never removes a lock and never waits for
// one. It is best-effort by design: dpkg, apt, and rpm guard their
// databases with fcntl locks the flock probe cannot observe, so the
// post-hoc manager output matching (isLockOutput) remains the reliable
// signal.
func lockHeld(system System, manager string) bool {
	for _, path := range managerLockPaths(manager) {
		held, err := probeHostLock(filepath.Join(system.Root, path))
		if err == nil && held {
			return true
		}
	}
	return false
}

// applyCommand is the manager's single-scope apply invocation. apt wraps
// the call through env(1) so DEBIAN_FRONTEND=noninteractive reaches
// apt-get without changing the shared Runner contract.
func applyCommand(manager string, items []UpdateItem) (string, []string) {
	switch manager {
	case "apt":
		args := []string{"DEBIAN_FRONTEND=noninteractive", "apt-get", "-y", "install"}
		for _, item := range items {
			if item.TargetVersion != "" && item.TargetVersion != item.CurrentVersion {
				args = append(args, item.Name+"="+item.TargetVersion)
			} else {
				args = append(args, item.Name)
			}
		}
		return "env", args
	case "dnf":
		args := []string{"-y", "upgrade"}
		for _, item := range items {
			if item.TargetVersion != "" && item.TargetVersion != item.CurrentVersion {
				args = append(args, item.Name+"-"+item.TargetVersion)
			} else {
				args = append(args, item.Name)
			}
		}
		return "dnf", args
	case "zypper":
		args := []string{"-n", "in"}
		for _, item := range items {
			if item.TargetVersion != "" && item.TargetVersion != item.CurrentVersion {
				args = append(args, item.Name+"="+item.TargetVersion)
			} else {
				args = append(args, item.Name)
			}
		}
		return "zypper", args
	case "apk":
		args := []string{"add"}
		for _, item := range items {
			if item.TargetVersion != "" && item.TargetVersion != item.CurrentVersion {
				args = append(args, item.Name+"="+item.TargetVersion)
			} else {
				args = append(args, item.Name)
			}
		}
		return "apk", args
	case "pacman":
		args := []string{"-S", "--noconfirm"}
		for _, item := range items {
			args = append(args, item.Name)
		}
		return "pacman", args
	}
	return "", nil
}

// ApplyUpdates executes one previewed update. Operations abandoned by a dead
// process are reclaimed before the active-operation check, the apply lock is
// held from before the operation is recorded until its terminal outcome is
// persisted, and the captured package state is revalidated immediately
// before execution. Every terminal outcome is persisted before the caller
// sees it.
func ApplyUpdates(ctx context.Context, system System, host Host, packages []string, token string, dataDir string) (UpdateResult, error) {
	result := UpdateResult{Manager: host.Manager, StartedAt: time.Now().UTC()}
	if err := validatePackageList(packages); err != nil {
		return result, err
	}
	if token == "" {
		return result, phelixerr.New(phelixerr.CodeHostSecurityInvalid, "a preview token is required")
	}
	if !SupportedRemediationManager(host.Manager) {
		return result, phelixerr.Newf(phelixerr.CodeHostSecurityUnsupported,
			"package manager %q has no update adapter", host.Manager)
	}
	if err := requirePrivilege(); err != nil {
		return result, err
	}
	if _, err := loadOperations(dataDir); err != nil {
		return result, err
	}
	execLock, lockErr := acquireApplyLock(dataDir)
	if lockErr != nil {
		if errors.Is(lockErr, ErrHostScanLockHeld) {
			// Fail closed: another process is executing an update and
			// its outcome is not yet known. Proceeding here could run a
			// second manager transaction against a stale preview.
			return result, phelixerr.New(phelixerr.CodeHostSecurityBusy,
				"another update operation is executing on this host; wait for it to finish")
		}
		return result, lockErr
	}
	defer func() {
		_ = releaseHostLock(execLock)
		_ = execLock.Close()
	}()
	ctx, cancel := context.WithTimeout(ctx, ApplyTimeout)
	defer cancel()

	names := packageNames(packages)
	var preview *storedPreview
	err := mutateOperations(dataDir, func(data *operationsFile) error {
		for _, active := range data.Operations {
			if active.Status == OperationStatusExecuting {
				return phelixerr.New(phelixerr.CodeHostSecurityInvalid,
					"another update operation is active; wait for it to finish or verify it")
			}
		}
		for i := range data.Previews {
			if data.Previews[i].Token == token {
				preview = &data.Previews[i]
				break
			}
		}
		if preview == nil {
			return phelixerr.New(phelixerr.CodeHostSecurityNotFound, "preview token is unknown; run a preview first")
		}
		if !preview.ExpiresAt.After(result.StartedAt) {
			return phelixerr.New(phelixerr.CodeHostSecurityPreviewExpired,
				"preview token expired; run a new preview")
		}
		requested := append([]string(nil), names...)
		sort.Strings(requested)
		bound := append([]string(nil), preview.Packages...)
		sort.Strings(bound)
		if len(requested) != len(bound) {
			return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "packages do not match the previewed set")
		}
		for i := range requested {
			if requested[i] != bound[i] {
				return phelixerr.New(phelixerr.CodeHostSecurityInvalid, "packages do not match the previewed set")
			}
		}
		operationID := "hso_" + uuid.NewString()
		result.OperationID = operationID
		data.Operations = append(data.Operations, storedOperation{
			OperationID: operationID, Type: "update_apply", Status: OperationStatusExecuting,
			Packages: names, PreviewToken: token, Manager: host.Manager,
			CreatedAt: result.StartedAt, UpdatedAt: result.StartedAt,
		})
		return nil
	})
	if err != nil {
		return result, err
	}
	finish := func(status, summary, errText string) error {
		result.Status = status
		result.Summary = summary
		result.Error = errText
		result.FinishedAt = time.Now().UTC()
		return mutateOperations(dataDir, func(data *operationsFile) error {
			for i := range data.Operations {
				if data.Operations[i].OperationID == result.OperationID {
					data.Operations[i].Status = status
					data.Operations[i].Result = &result
					data.Operations[i].UpdatedAt = result.FinishedAt
					break
				}
			}
			return nil
		})
	}

	if lockHeld(system, host.Manager) {
		err := phelixerr.New(phelixerr.CodeHostSecurityBusy,
			"the package manager database is locked by another process; nothing was removed or waited out")
		if persistErr := finish(OperationStatusFailed, "", err.Error()); persistErr != nil {
			return result, errors.Join(err, persistErr)
		}
		return result, err
	}

	versions, err := currentVersions(ctx, system, host.Manager, preview.Packages)
	if err != nil {
		if persistErr := finish(OperationStatusFailed, "", err.Error()); persistErr != nil {
			return result, errors.Join(err, persistErr)
		}
		return result, err
	}
	entries := make([][2]string, 0, len(preview.Packages))
	for _, name := range preview.Packages {
		entries = append(entries, [2]string{name, versions[name]})
	}
	if stateHashOf(entries) != preview.StateHash {
		err := phelixerr.New(phelixerr.CodeHostSecurityPreviewStale,
			"package state changed since the preview; run a new preview")
		if persistErr := finish(OperationStatusFailed, "", err.Error()); persistErr != nil {
			return result, errors.Join(err, persistErr)
		}
		return result, err
	}

	name, args := applyCommand(host.Manager, preview.Items)
	raw, runErr := system.Runner.Run(ctx, name, args...)
	if runErr != nil {
		message := phelixerr.Redact(runErr.Error())
		if isLockOutput(raw) {
			err = phelixerr.New(phelixerr.CodeHostSecurityBusy,
				"the package manager database is locked by another process; nothing was removed or waited out")
		} else if ctx.Err() != nil {
			err = phelixerr.New(phelixerr.CodeHostSecurityTimeout, "the update exceeded its time budget")
		} else {
			err = phelixerr.Wrap(phelixerr.CodeHostSecurityExecFailed, "the package manager update failed", runErr)
		}
		if persistErr := finish(OperationStatusFailed, "", message); persistErr != nil {
			return result, errors.Join(err, persistErr)
		}
		return result, err
	}

	updated := 0
	for _, item := range preview.Items {
		if item.TargetVersion != item.CurrentVersion && item.TargetVersion != "" {
			updated++
		}
	}
	if state, known := DetectReboot(ctx, system, host); known {
		result.RebootRequired = state.Required
	}
	summary := fmt.Sprintf("update via %s finished for %d package(s), %d changed", host.Manager, len(preview.Items), updated)
	if persistErr := finish(OperationStatusSucceeded, summary, ""); persistErr != nil {
		return result, persistErr
	}
	return result, nil
}

// isLockOutput reports whether command output carries a manager lock
// complaint.
func isLockOutput(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, pattern := range managerLockPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// StoredPreview loads one durable preview by token; an empty token returns
// the newest stored preview. Expiry is not enforced here: apply rechecks
// it.
func StoredPreview(dataDir, token string) (Preview, error) {
	state, err := loadOperations(dataDir)
	if err != nil {
		return Preview{}, err
	}
	var newest *storedPreview
	for i := range state.Previews {
		preview := &state.Previews[i]
		if token != "" {
			if preview.Token == token {
				return storedPreviewToPreview(preview), nil
			}
			continue
		}
		if newest == nil || preview.CreatedAt.After(newest.CreatedAt) {
			newest = preview
		}
	}
	if newest == nil {
		return Preview{}, phelixerr.New(phelixerr.CodeHostSecurityNotFound,
			"no stored preview found; run 'phelix security server preview' first")
	}
	return storedPreviewToPreview(newest), nil
}

// storedPreviewToPreview converts the durable preview record to its API
// form.
func storedPreviewToPreview(preview *storedPreview) Preview {
	items := preview.Items
	if items == nil {
		items = []UpdateItem{}
	}
	return Preview{
		Token: preview.Token, ExpiresAt: preview.ExpiresAt, Manager: preview.Manager,
		Items: items, Warnings: []string{}, Limitations: previewLimitations(preview.Manager),
		CreatedAt: preview.CreatedAt,
	}
}

// Verify re-scans the host and maps each requested package's remaining
// findings. The fresh scan record is the only verification evidence: exit
// codes never verify an operation. The verification scan runs under the
// non-blocking host scan lock; a held lock skips the live scan and reports
// busy, leaving the operation free for a later retry.
func Verify(ctx context.Context, system System, store *Store, serverID, operationID, dataDir string, intel *Intelligence) (Scan, []VerificationEntry, error) {
	entries := []VerificationEntry{}
	empty := Scan{}
	if operationID == "" {
		return empty, entries, phelixerr.New(phelixerr.CodeHostSecurityInvalid, "an operation id is required")
	}
	operations, err := loadOperations(dataDir)
	if err != nil {
		return empty, entries, err
	}
	var operation *storedOperation
	for i := range operations.Operations {
		if operations.Operations[i].OperationID == operationID {
			operation = &operations.Operations[i]
			break
		}
	}
	if operation == nil {
		return empty, entries, phelixerr.New(phelixerr.CodeHostSecurityNotFound, "operation is unknown")
	}
	if operation.Status != OperationStatusSucceeded && operation.Status != OperationStatusFailed {
		return empty, entries, phelixerr.New(phelixerr.CodeHostSecurityInvalid,
			"the operation has not finished; verification needs a terminal apply result")
	}

	lock, lockErr := TryAcquireHostScanLock(dataDir)
	if errors.Is(lockErr, ErrHostScanLockHeld) {
		return empty, entries, phelixerr.New(phelixerr.CodeHostSecurityScanRunning,
			"a host security scan is already running; retry verification after it completes")
	}
	if lockErr != nil {
		return empty, entries, lockErr
	}
	defer lock.Release()

	scan, scanErr := ScanHost(ctx, system, store, serverID, nil, intel)
	if scanErr != nil {
		return empty, entries, phelixerr.Wrap(phelixerr.CodeHostSecurityExecFailed, "the verification scan failed", scanErr)
	}
	remaining := map[string][]string{}
	for _, finding := range scan.Findings {
		remaining[finding.Package] = append(remaining[finding.Package], finding.CVE)
	}
	for _, spec := range operation.Packages {
		name, _ := SplitPackageSpec(spec)
		entry := VerificationEntry{Package: name}
		if cves := remaining[name]; len(cves) > 0 {
			sort.Strings(cves)
			entry.Detail = "remaining findings: " + strings.Join(cves, ", ")
		} else {
			entry.Resolved = true
			entry.Detail = "no remaining findings for this package"
		}
		entries = append(entries, entry)
	}
	result := UpdateResult{
		OperationID: operation.OperationID, Status: OperationStatusVerified, Manager: operation.Manager,
		Summary: "verified against scan " + scan.ID, Verification: entries,
		RebootRequired: scan.RebootRequired, StartedAt: scan.StartedAt, FinishedAt: scan.FinishedAt,
	}
	verifiedAt := time.Now().UTC()
	if err := mutateOperations(dataDir, func(data *operationsFile) error {
		for i := range data.Operations {
			if data.Operations[i].OperationID == operationID {
				data.Operations[i].Status = OperationStatusVerified
				data.Operations[i].Result = &result
				data.Operations[i].UpdatedAt = verifiedAt
				break
			}
		}
		return nil
	}); err != nil {
		return scan, entries, err
	}
	return scan, entries, nil
}
