package cmd

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/workspace"
	"github.com/fatih/color"
)

// workspaceFromCWD resolves the workspace rooted at the current directory,
// where the manifest (phelix.workspace.yaml) or a native go.work / Cargo
// workspace must live. There is no ancestor search on purpose: like
// phelix.yaml, the workspace definition is read from exactly one place so
// commands are predictable.
func workspaceFromCWD() (*workspace.Workspace, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
	}
	return workspace.Resolve(dir)
}

// loadWorkspaceState returns the persisted workspace state for ws, creating
// an empty record seeded with the current membership when none exists.
func loadWorkspaceState(ws *workspace.Workspace) (*workspace.State, error) {
	state, err := workspace.LoadState(ws.Name)
	if err != nil {
		return nil, err
	}
	if state == nil {
		state = &workspace.State{}
	}
	state.SyncMembership(ws)
	return state, nil
}

// runParallelLevel runs fn over the service names of one dependency level
// with a bounded worker pool. The first failure cancels work that has not
// started yet; in-flight work always finishes so no service is left
// half-built. It returns the first error, or nil when the level succeeded.
func runParallelLevel(names []string, concurrency int, fn func(string) error) error {
	if len(names) == 0 {
		return nil
	}
	if concurrency < 1 {
		concurrency = 1
	}
	workers := concurrency
	if workers > len(names) {
		workers = len(names)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, len(names))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(service string) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()
			if err := fn(service); err != nil {
				errs <- err
				cancel()
			}
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

// printWorkspaceHeader emits the shared workspace banner line.
func printWorkspaceHeader(ws *workspace.Workspace, action string) {
	fmt.Printf("%s Workspace %s: %s %d service(s) (source: %s)\n",
		color.BlueString("→"), color.CyanString(ws.Name), action, len(ws.Services), ws.Source)
}

// loadAppStateForWorkspace loads the app registry before workspace
// orchestration. It is a function seam so tests can seed app state directly:
// the app package pins its on-disk state path at package init, which an
// in-process test cannot redirect.
var loadAppStateForWorkspace = func() error {
	return app.Manager.LoadState()
}

// findRegisteredApp looks a workspace service up in app state without
// failing when it was never built.
func findRegisteredApp(name string) *app.AppInfo {
	if info, err := GetAppInfo(name); err == nil {
		return info
	}
	return nil
}
