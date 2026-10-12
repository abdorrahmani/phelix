package workspace

// MemberSource records where a workspace member came from, for `workspace list`
// (Phase 1).
type MemberSource string

const (
	// SourceManifest: the member is declared in phelix.workspace.yaml services.
	SourceManifest MemberSource = "manifest"
	// SourceGoWork: discovered from a go.work use directive.
	SourceGoWork MemberSource = "go.work"
	// SourceCargo: discovered from Cargo [workspace].members.
	SourceCargo MemberSource = "cargo"
	// SourceInclude: added by a discovery.include glob.
	SourceInclude MemberSource = "include"
)

// ResolvedService is one workspace member after discovery, precedence
// resolution and validation (Phase 1 populates it; Phase 0 only declares it).
// Port is the public proxy port — distinct from the per-slot internal instance
// port the deploy engine assigns and from the PORT env var Phelix injects.
type ResolvedService struct {
	Name      string
	Path      string // workspace-root-relative, slash-cleaned
	AbsPath   string // absolute on-disk path
	Port      int
	Strategy  string
	Runtime   string
	Replicas  int
	DependsOn []string
	Health    *HealthSpec
	Build     *ServiceBuild
	Source    MemberSource
}

// ResolvedWorkspace is the fully resolved mesh: the workspace identity plus its
// members (the Phase 1 resolver applies a stable, sorted order).
//
// Live per-service runtime state is never read from here, nor from
// workspace.json. It is always derived from each service's own deploy.json
// (internal/deploy) — the invariant the status/rollback phases rely on.
type ResolvedWorkspace struct {
	Name     string
	Root     string
	Services []ResolvedService
}
