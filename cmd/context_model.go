package cmd

// context_model.go builds the Phase 2 inspection/context views. Every builder
// is a projection over state Phelix already maintains (project loader, doctor
// checks, app manager, deploy.json, versions.json, the health config/daemon,
// the capability registry, the operation records) — no second discovery
// mechanism exists anywhere in this file.
//
// Rules carried over from the Phase 1 machine contract:
//   - facts only, no recommendations (decision-making is a future layer);
//   - every collection bounded, truncation explicit;
//   - free-text messages pass through the centralized redactor;
//   - persisted state is labeled as such, live checks as live;
//   - slices, never raw maps, so JSON key order is deterministic.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/docker"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/health"
	phelixlogs "github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/toolchain"
)

// Context bounds. Defaults sized for an agent's context window; flags may
// lower them but never exceed the caps (resource-exhaustion guard).
const (
	contextDefaultVersions   = 20
	contextDefaultOperations = 20
	contextDefaultLogLines   = 100
	contextMaxVersions       = 100
	contextMaxOperations     = 100
	contextMaxLogLines       = 1000
)

// validateContextLimit applies the shared limit semantics: <= 0 means "use
// the default", values above the cap are rejected instead of silently
// clamped (an agent asking for 1e9 versions is misusing the interface and
// must hear about it).
func validateContextLimit(name string, value, def, cap int) (int, error) {
	if value < 0 {
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "--%s must be >= 0 (0 = default %d), got %d", name, def, value)
	}
	if value > cap {
		return 0, phelixerr.Newf(phelixerr.CodeInvalidArgument, "--%s must be <= %d, got %d", name, cap, value)
	}
	if value == 0 {
		return def, nil
	}
	return value, nil
}

// --- project -----------------------------------------------------------------

type inspectProject struct {
	Root          string `json:"root"`
	Name          string `json:"name,omitempty"`
	Language      string `json:"language,omitempty"`
	ConfigPath    string `json:"config_path,omitempty"`
	ConfigFound   bool   `json:"config_found"`
	ConfigValid   bool   `json:"config_valid"`
	ConfigError   string `json:"config_error,omitempty"`
	Port          int    `json:"port,omitempty"`
	ReadsPort     bool   `json:"reads_port,omitempty"`
	HardcodedPort *int   `json:"hardcoded_port,omitempty"`
}

// buildProjectContext inspects dir with the same primitives doctor uses
// (builder detection + project.Load + ScanHardcodedPort). A missing
// phelix.yaml is a fact (config_found=false), not an error; a malformed one
// is reported with its redacted error. A directory that is not a detectable
// Go/Rust project at all yields NOT_FOUND — there is nothing to inspect.
func buildProjectContext(dir string) (*inspectProject, error) {
	buildMgr := builder.NewBuildManager()
	lang := buildMgr.DetectLanguage(dir)
	if !lang.IsSupported() {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound,
			"no Go or Rust project detected in %s (a go.mod or Cargo.toml is required)", dir)
	}

	view := &inspectProject{
		Root:     dir,
		Language: string(lang),
	}

	cfg, cfgErr := project.Load(dir)
	switch {
	case cfgErr == nil:
		view.ConfigFound = true
		view.ConfigValid = true
		view.ConfigPath = filepath.Join(dir, project.FileName)
		view.Name = cfg.Name
		view.Port = cfg.Port
	case phelixerr.AsError(cfgErr) != nil && phelixerr.AsError(cfgErr).Code == phelixerr.CodeNotFound:
		view.ConfigFound = false
		view.ConfigPath = filepath.Join(dir, project.FileName)
	default:
		view.ConfigFound = true
		view.ConfigValid = false
		view.ConfigPath = filepath.Join(dir, project.FileName)
		view.ConfigError = phelixerr.Redact(cfgErr.Error())
	}

	hits, readsPORT := project.ScanHardcodedPort(dir)
	view.ReadsPort = readsPORT
	if len(hits) > 0 {
		p := hits[0].Port
		view.HardcodedPort = &p
	}
	return view, nil
}

// --- runtime / toolchain -------------------------------------------------------

type toolchainView struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

type inspectRuntime struct {
	OS           string         `json:"os"`
	Architecture string         `json:"architecture"`
	Go           *toolchainView `json:"go,omitempty"`
	Rust         *toolchainView `json:"rust,omitempty"`
	Docker       *toolchainView `json:"docker,omitempty"`
}

// buildRuntimeContext reports the toolchains Phelix actually checks and
// depends on (doctor's detection for Go/Rust, the dockerize path's check for
// Docker). Facts only — installed and version, never recommendations.
func buildRuntimeContext() *inspectRuntime {
	view := &inspectRuntime{
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		Go:           toolchainContextView(builder.Go),
		Rust:         toolchainContextView(builder.Rust),
		Docker:       dockerContextView(),
	}
	return view
}

func toolchainContextView(lang builder.Language) *toolchainView {
	if !toolchain.IsInstalled(lang) {
		return &toolchainView{Installed: false}
	}
	return &toolchainView{Installed: true, Version: toolchainVersion(lang)}
}

func dockerContextView() *toolchainView {
	if err := docker.CheckDockerAvailable(); err == nil {
		return &toolchainView{Installed: true, Version: dockerVersion()}
	}
	return &toolchainView{Installed: false}
}

// dockerVersion reports the docker CLI version string — the same binary the
// dockerize path shells out to. The output is docker-authored, but it passes
// through the centralized redactor on principle: no context path is exempt.
func dockerVersion() string {
	out, err := exec.Command("docker", "--version").Output()
	if err != nil {
		return "installed"
	}
	return phelixerr.Redact(strings.TrimSpace(string(out)))
}

// --- effective configuration ---------------------------------------------------

type deployCfgView struct {
	Strategy string `json:"strategy,omitempty"`
	Replicas int    `json:"replicas,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	Network  string `json:"network,omitempty"`
}

type resourcesCfgView struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type webhookCfgView struct {
	Branch    string `json:"branch,omitempty"`
	SecretEnv string `json:"secret_env,omitempty"`
}

type inspectConfig struct {
	Source          string               `json:"source"`
	Name            string               `json:"name,omitempty"`
	Port            int                  `json:"port,omitempty"`
	Deploy          *deployCfgView       `json:"deploy,omitempty"`
	Resources       *resourcesCfgView    `json:"resources,omitempty"`
	HealthEndpoints []healthEndpointView `json:"health_endpoints,omitempty"`
	Watching        string               `json:"watching,omitempty"`
	MatrixEnabled   bool                 `json:"matrix_enabled"`
	Webhook         *webhookCfgView      `json:"webhook,omitempty"`
}

// buildConfigContext projects the effective phelix.yaml onto the contract.
// Deliberately selected fields only — never a raw config dump — and never a
// secret: the webhook section exposes only the NAME of the env var holding
// the HMAC secret, never its value.
func buildConfigContext(cfg *project.Config) *inspectConfig {
	if cfg == nil {
		return &inspectConfig{Source: "defaults"}
	}
	view := &inspectConfig{
		Source: "phelix.yaml",
		Name:   cfg.Name,
		Port:   cfg.Port,
	}
	if cfg.Deploy != nil {
		view.Deploy = &deployCfgView{
			Strategy: cfg.Deploy.Strategy,
			Replicas: cfg.Deploy.Replicas,
			Runtime:  cfg.DeployRuntime(),
			Network:  cfg.DeployNetwork(),
		}
	}
	if !cfg.Resources.IsZero() {
		view.Resources = &resourcesCfgView{CPU: cfg.Resources.CPU, Memory: cfg.Resources.Memory}
	}
	if cfg.Health != nil && len(cfg.Health.Endpoints) > 0 {
		view.HealthEndpoints = make([]healthEndpointView, 0, len(cfg.Health.Endpoints))
		for _, ep := range cfg.Health.Endpoints {
			view.HealthEndpoints = append(view.HealthEndpoints, healthEndpointView{
				Name:     ep.Name,
				URL:      ep.Path,
				Interval: ep.Interval,
				Retries:  ep.Retries,
				Timeout:  ep.Mode,
			})
		}
	}
	view.Watching = cfg.Watching
	if cfg.Matrix != nil {
		view.MatrixEnabled = cfg.Matrix.Enabled
	}
	if cfg.Webhook != nil {
		view.Webhook = &webhookCfgView{
			Branch:    cfg.Webhook.Branch,
			SecretEnv: cfg.Webhook.SecretEnv,
		}
	}
	return view
}

// --- application state ----------------------------------------------------------

type proxyView struct {
	Running    bool `json:"running"`
	Enrolled   bool `json:"enrolled"`
	PublicPort int  `json:"public_port,omitempty"`
}

type inspectApp struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Language   string     `json:"language,omitempty"`
	Status     string     `json:"status"`
	Watching   bool       `json:"watching"`
	PID        int        `json:"pid,omitempty"`
	Uptime     string     `json:"uptime,omitempty"`
	RAMUsageMB float64    `json:"ram_usage_mb,omitempty"`
	CPUUsage   float64    `json:"cpu_usage_percent,omitempty"`
	Version    int        `json:"version,omitempty"`
	Tag        string     `json:"tag,omitempty"`
	DeployMode string     `json:"deploy_mode,omitempty"`
	ActiveSlot string     `json:"active_slot,omitempty"`
	Replicas   int        `json:"replicas,omitempty"`
	Proxy      *proxyView `json:"proxy,omitempty"`
	// Source is "live": the reconciled status is verified against the actual
	// process (PID liveness), not merely read back from state files.
	Source string `json:"source"`
}

// buildAppContext reuses the status command's exact reconciliation path
// (reconcileAppWithDeploy → StatusApplication), so "running" here means the
// same thing it means in `phelix status`.
func buildAppContext(status app.AppStatus, proxyUp bool, proxyByApp map[string]proxy.AppStatus) *inspectApp {
	view := &inspectApp{
		ID:         status.ID,
		Name:       status.Name,
		Language:   status.Language,
		Status:     status.Status,
		Watching:   status.Watching,
		PID:        status.PID,
		Uptime:     status.Uptime,
		RAMUsageMB: float64(status.RAMUsage) / (1024 * 1024),
		CPUUsage:   status.CPUUsage,
		Source:     "live",
	}
	if meta, err := deploy.CurrentVersionMeta(status.Name); err == nil && meta != nil {
		view.Version = meta.Version
		view.Tag = meta.Tag
	}
	if state, err := deploy.Load(status.Name); err == nil && state != nil && state.Mode != "" {
		view.DeployMode = string(state.Mode)
		view.ActiveSlot = state.ActiveSlot
		view.Replicas = len(state.Replicas)
	}
	pv := &proxyView{Running: proxyUp}
	if ps, ok := proxyByApp[status.Name]; ok {
		pv.Enrolled = true
		pv.PublicPort = ps.PublicPort
	}
	view.Proxy = pv
	return view
}

// --- deployment state -------------------------------------------------------------

type opLockView struct {
	Operation string `json:"operation"`
	PID       int    `json:"pid,omitempty"`
	StartedAt int64  `json:"started_at_ms"`
}

type canaryView struct {
	Status   string `json:"status"`
	Strategy string `json:"strategy,omitempty"`
	Version  int    `json:"version,omitempty"`
	Step     int    `json:"step,omitempty"`
	Steps    int    `json:"steps,omitempty"`
}

type inspectDeployment struct {
	DeploymentID  string       `json:"deployment_id,omitempty"`
	Status        string       `json:"status"`
	Strategy      string       `json:"strategy,omitempty"`
	ActiveVersion int          `json:"active_version,omitempty"`
	PublicPort    int          `json:"public_port,omitempty"`
	ActiveSlot    string       `json:"active_slot,omitempty"`
	ServingAlive  *bool        `json:"serving_alive,omitempty"`
	OpLock        *opLockView  `json:"op_lock,omitempty"`
	Canary        *canaryView  `json:"canary,omitempty"`
	LastRollback  *rollbackRef `json:"last_rollback,omitempty"`
	UpdatedAt     int64        `json:"updated_at_ms,omitempty"`
	// Source is "persisted": the section reads deploy.json. The one live
	// fact it carries is serving_alive (PID liveness of the serving instance).
	Source string `json:"source"`
}

type rollbackRef struct {
	FromVersion int   `json:"from_version"`
	ToVersion   int   `json:"to_version"`
	At          int64 `json:"at_ms"`
}

// buildDeploymentContext projects deploy.json. Status uses the external
// lifecycle vocabulary: an in-flight op lock means running, a persisted
// topology means the last deployment succeeded, no state at all means
// nothing has been deployed (pending).
func buildDeploymentContext(state *deploy.DeployState) *inspectDeployment {
	if state == nil {
		return &inspectDeployment{Status: machine.StatusPending, Source: "persisted"}
	}
	view := &inspectDeployment{
		DeploymentID:  state.LastDeploymentID,
		Status:        machine.StatusSucceeded,
		Strategy:      string(state.Mode),
		ActiveVersion: state.ActiveVersion,
		PublicPort:    state.PublicPort,
		ActiveSlot:    state.ActiveSlot,
		UpdatedAt:     state.UpdatedAt.UnixMilli(),
		Source:        "persisted",
	}
	if state.OpLock != nil {
		// A deploy/rollback is in flight right now — the live fact wins over
		// the persisted "succeeded" of the previous deployment.
		view.Status = machine.StatusRunning
		view.OpLock = &opLockView{
			Operation: state.OpLock.Operation,
			PID:       state.OpLock.PID,
			StartedAt: state.OpLock.StartedAt.UnixMilli(),
		}
	}
	if state.Canary != nil {
		view.Canary = &canaryView{
			Status:   string(state.Canary.Status),
			Strategy: state.Canary.Strategy,
			Version:  state.Canary.Version,
			Step:     state.Canary.Step,
			Steps:    state.Canary.Steps,
		}
	}
	if state.LastRollback != nil {
		view.LastRollback = &rollbackRef{
			FromVersion: state.LastRollback.FromVersion,
			ToVersion:   state.LastRollback.ToVersion,
			At:          state.LastRollback.At.UnixMilli(),
		}
	}
	if inst := state.ServingInstance(); inst != nil {
		alive := deploy.InstanceAlive(inst)
		view.ServingAlive = &alive
	}
	return view
}

// --- versions ---------------------------------------------------------------------

type versionView struct {
	Version    int    `json:"version"`
	Tag        string `json:"tag,omitempty"`
	Commit     string `json:"commit,omitempty"`
	BuiltAt    int64  `json:"built_at_ms,omitempty"`
	DeployedAt int64  `json:"deployed_at_ms,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	IsCurrent  bool   `json:"current"`
	DeployMode string `json:"deploy_mode,omitempty"`
}

type inspectVersions struct {
	Current   *versionView  `json:"current,omitempty"`
	Available []versionView `json:"available"`
	Count     int           `json:"count"`
	Truncated bool          `json:"truncated"`
	Limit     int           `json:"limit"`
}

// buildVersionsContext projects versions.json bounded by limit. The bound is
// a real bound (the underlying store keeps everything), so truncation is
// explicit rather than fake pagination.
func buildVersionsContext(appName string, limit int) (*inspectVersions, error) {
	view := &inspectVersions{Available: []versionView{}, Limit: limit}
	if meta, err := deploy.CurrentVersionMeta(appName); err == nil && meta != nil {
		view.Current = versionViewFromMeta(*meta)
	}
	vers, err := deploy.ListVersionsForDisplay(appName, deploy.DefaultRetention{Max: 5})
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to list versions", err)
	}
	total := len(vers)
	if total > limit {
		vers = vers[:limit]
		view.Truncated = true
	}
	for _, v := range vers {
		view.Available = append(view.Available, *versionViewFromMeta(v))
	}
	view.Count = len(view.Available)
	return view, nil
}

func versionViewFromMeta(meta deploy.VersionMeta) *versionView {
	v := &versionView{
		Version:    meta.Version,
		Tag:        meta.Tag,
		Commit:     meta.GitCommit,
		BuiltAt:    meta.BuiltAt.UnixMilli(),
		SizeBytes:  meta.SizeBytes,
		IsCurrent:  meta.IsCurrent,
		DeployMode: meta.DeployMode,
	}
	if meta.DeployedAt != nil {
		v.DeployedAt = meta.DeployedAt.UnixMilli()
	}
	return v
}

// --- health -----------------------------------------------------------------------

// Health status vocabulary for the context contract (distinct from the
// operation lifecycle vocabulary): healthy | degraded | unhealthy | unknown.
const (
	HealthStatusHealthy   = "healthy"
	HealthStatusDegraded  = "degraded"
	HealthStatusUnhealthy = "unhealthy"
	HealthStatusUnknown   = "unknown"
)

type inspectHealth struct {
	Status     string            `json:"status"`
	Source     string            `json:"source"` // "daemon" | "oneshot" | "none"
	DeployTier string            `json:"deploy_tier,omitempty"`
	Checks     []healthCheckView `json:"checks,omitempty"`
}

// buildHealthContext gathers check results from the running health daemon
// when one exists, otherwise runs a one-shot pass with the same checker. Map
// keys are sorted into slices for deterministic output; messages are
// redacted.
func buildHealthContext(appID, appName string) (*inspectHealth, error) {
	view := &inspectHealth{Status: HealthStatusUnknown, Source: "none"}

	if statuses, ok := globalDaemonStatuses(appID); ok {
		view.Source = "daemon"
		view.Checks = sortedHealthChecks(healthCheckViewsFrom(statuses))
	} else {
		configMgr, err := health.InitConfigManager()
		if err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize health config", err)
		}
		config := configMgr.GetConfig(appID)
		if config == nil || len(config.Endpoints) == 0 {
			return view, nil
		}
		view.Source = "oneshot"
		checker := health.NewChecker()
		for name, ep := range config.Endpoints {
			result := checker.Check(ep)
			view.Checks = append(view.Checks, healthCheckView{
				Name:       name,
				URL:        result.URL,
				Status:     result.Status,
				HTTPStatus: result.StatusCode,
				LatencyMs:  result.LatencyMs,
				Error:      redactHealthMessage(result.Error),
				CheckedAt:  result.CheckedAt.UnixMilli(),
			})
		}
		view.Checks = sortedHealthChecks(view.Checks)
	}

	if state, err := deploy.Load(appName); err == nil && state != nil && state.Health != nil {
		view.DeployTier = state.Health.TierLabel
	}

	view.Status = rollupHealthStatus(view.Checks, view.Source)
	return view, nil
}

// globalDaemonStatuses probes the running health daemon without paying its
// startup costs: the global singleton PANICS when it was never initialized
// (every fresh CLI process), and GetDaemon blocks up to 5 seconds waiting for
// a daemon that may never come. A context query must be bounded, so the
// probe runs on a goroutine with a short deadline and recovers the
// not-initialized panic; falling back to the one-shot checker is the
// documented behavior (source: "oneshot").
func globalDaemonStatuses(appID string) (statuses map[string]*health.HealthCheckResult, ok bool) {
	type probe struct {
		statuses map[string]*health.HealthCheckResult
		ok       bool
	}
	done := make(chan probe, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- probe{nil, false}
			}
		}()
		gd := health.GetGlobalDaemon()
		daemon, err := gd.GetDaemon()
		if err != nil {
			done <- probe{nil, false}
			return
		}
		done <- probe{daemon.GetStatus(appID), true}
	}()
	select {
	case r := <-done:
		return r.statuses, r.ok
	case <-time.After(200 * time.Millisecond):
		return nil, false
	}
}

func sortedHealthChecks(checks []healthCheckView) []healthCheckView {
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	return checks
}

// rollupHealthStatus derives the section status from the check results: all
// UP is healthy, any DOWN is unhealthy, anything else observed is degraded,
// and no checks at all is unknown.
func rollupHealthStatus(checks []healthCheckView, source string) string {
	if len(checks) == 0 {
		return HealthStatusUnknown
	}
	up, down := 0, 0
	for _, c := range checks {
		switch c.Status {
		case "UP":
			up++
		case "DOWN":
			down++
		}
	}
	switch {
	case down > 0:
		return HealthStatusUnhealthy
	case up == len(checks):
		return HealthStatusHealthy
	default:
		return HealthStatusDegraded
	}
}

func redactHealthMessage(msg *string) *string {
	if msg == nil {
		return nil
	}
	redacted := phelixerr.Redact(*msg)
	return &redacted
}

// --- capabilities -------------------------------------------------------------------

type inspectCapabilities struct {
	Capabilities []string `json:"capabilities"`
}

// buildCapabilitiesContext exposes the existing capability registry (the same
// source the backend's CLIMetadata consumes). The registry already returns a
// deterministically sorted list.
func buildCapabilitiesContext() *inspectCapabilities {
	return &inspectCapabilities{Capabilities: phelixgrpc.AgentCapabilities()}
}

// --- operations ----------------------------------------------------------------------

type inspectOperationsView struct {
	Operations []operationView `json:"operations"`
	Count      int             `json:"count"`
	Truncated  bool            `json:"truncated"`
	Limit      int             `json:"limit"`
}

// buildOperationsContext lists the Phase 1 operation records bounded. The
// underlying store is bounded by the limit; truncation is explicit.
func buildOperationsContext(appName string, limit int) (*inspectOperationsView, error) {
	recs, skipped, err := ops.List(appName, limit+1) // one past the limit to detect truncation
	if err != nil {
		return nil, err
	}
	view := &inspectOperationsView{Operations: []operationView{}, Limit: limit}
	if skipped > 0 {
		// Corrupt records are a fact worth surfacing, but never a failure of
		// the query itself.
		phelixlogs.WarningFile("context", "%d corrupt operation record(s) skipped", skipped)
	}
	truncated := false
	if len(recs) > limit {
		recs = recs[:limit]
		truncated = true
	}
	for _, rec := range recs {
		view.Operations = append(view.Operations, operationViewFromRecord(rec))
	}
	view.Count = len(view.Operations)
	view.Truncated = truncated
	return view, nil
}

// --- recent logs ------------------------------------------------------------------------

// buildLogsContext reuses the Phase 1 bounded log reader (readLogHistory) and
// result shape, including its display-time redaction.
func buildLogsContext(spec logViewSpec, lines int) (*logResult, error) {
	f, err := os.Open(spec.Path)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to open log file %s", spec.Path)
	}
	defer f.Close()
	defer f.Close()

	items, total, err := readLogHistory(f, lines, nil)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read log file", err)
	}
	view := &logResult{
		Source:    spec.Source,
		App:       spec.Name,
		Path:      spec.Path,
		Items:     make([]string, 0, len(items)),
		Count:     len(items),
		Truncated: total > len(items),
	}
	for _, line := range items {
		view.Items = append(view.Items, phelixerr.Redact(line))
	}
	return view, nil
}

// --- composite ---------------------------------------------------------------------------

type contextResult struct {
	Project      *inspectProject        `json:"project,omitempty"`
	Runtime      *inspectRuntime        `json:"runtime,omitempty"`
	Config       *inspectConfig         `json:"config,omitempty"`
	Application  *inspectApp            `json:"application,omitempty"`
	Deployment   *inspectDeployment     `json:"deployment,omitempty"`
	Versions     *inspectVersions       `json:"versions,omitempty"`
	Health       *inspectHealth         `json:"health,omitempty"`
	Capabilities *inspectCapabilities   `json:"capabilities,omitempty"`
	Operations   *inspectOperationsView `json:"operations,omitempty"`
	Logs         *logResult             `json:"logs,omitempty"`
	// Truncated is true when ANY bounded section dropped data; an agent can
	// trust a false here to mean "the context is complete".
	Truncated bool `json:"truncated"`
}
