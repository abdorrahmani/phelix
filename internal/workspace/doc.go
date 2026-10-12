// Package workspace is the foundation of Phelix's workspace-mesh support: the
// phelix.workspace.yaml schema, its strict loader, the resolved-model
// vocabulary later phases populate, and the on-disk path model for workspace
// coordination state. It builds nothing else yet — no discovery, cross-field
// validation, dependency graph, lock, or commands; those are later phases that
// call these types directly.
//
// Locked product decisions (they drive later phases):
//   - Discovery is hybrid: members come from go.work use-dirs and Cargo
//     [workspace].members, overlaid and include/exclude-filtered by the
//     manifest, which may also stand alone with no go.work/Cargo present.
//   - A mesh deploy failure auto-rolls-back the whole run, all-or-nothing.
//   - State model is a coordinator record + flock lock under
//     <base>/workspaces/<name>/; each service's own deploy.json stays the
//     authoritative source for that service's live state.
//   - v1 command scope is the quartet build --all, deploy --all,
//     status --workspace, rollback --all. Group start/stop/restart, workspace
//     logs, and remote/backend-triggered group ops are non-goals.
//
// On-disk path model: workspace coordination state lives at
// <base>/workspaces/<SanitizeName(name)>/ as a sibling of the per-app
// <base>/apps/<App>/ tree, where <base> is the same base internal/deploy uses
// (os.UserHomeDir()/.phelix today). Planned files there: workspace.json (the
// coordinator record), workspace.lock (the flock file) and workspace.log (the
// append-only group-op audit). Phase 0 provides path helpers for all three and
// writes none of them.
//
// Invariant: workspace.json is group-operation context only — never the source
// of truth for a service's live runtime state, which is always derived from
// that service's deploy.json (see ResolvedWorkspace).
package workspace
