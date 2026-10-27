package cmd

import (
	"fmt"
	"os"
	"sort"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/workspace"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
)

// wsStatus switches `phelix status` into the whole-workspace overview.
var wsStatus bool

// wsStatusProbeTimeout caps every health probe in the workspace overview so
// one dead endpoint cannot stall a six-service status view.
const wsStatusProbeTimeout = "2s"

type workspaceStatusResult struct {
	Workspace string                       `json:"workspace"`
	Root      string                       `json:"root"`
	Source    string                       `json:"source"`
	Services  []workspaceServiceStatusView `json:"services"`
}

type workspaceServiceStatusView struct {
	Name       string                        `json:"name"`
	Path       string                        `json:"path"`
	Registered bool                          `json:"registered"`
	Status     string                        `json:"status"`
	Version    int                           `json:"version,omitempty"`
	Tag        string                        `json:"tag,omitempty"`
	Port       int                           `json:"port,omitempty"`
	Strategy   string                        `json:"strategy,omitempty"`
	DependsOn  []string                      `json:"depends_on,omitempty"`
	Health     string                        `json:"health,omitempty"`
	Endpoints  []workspaceEndpointHealthView `json:"endpoints,omitempty"`
}

type workspaceEndpointHealthView struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	Status string `json:"status"`
}

// runWorkspaceStatus renders the unified workspace overview: for every
// service, the derived lifecycle status, the current version, the public
// port, and a one-shot health probe of its configured endpoints. Runtime
// truth comes from the per-app stores exactly like `phelix status <app>`;
// the workspace only unifies the view.
func runWorkspaceStatus() error {
	ws, err := workspaceFromCWD()
	if err != nil {
		return err
	}
	if err := app.Manager.LoadState(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	views := make([]workspaceServiceStatusView, 0, len(ws.Services))
	for _, svc := range ws.Services {
		views = append(views, buildWorkspaceServiceStatus(svc))
	}
	result := workspaceStatusResult{Workspace: ws.Name, Root: ws.Root, Source: ws.Source, Services: views}
	if machine.Active() {
		return writeEnvelopeResult(machine.Success("", result))
	}
	renderWorkspaceStatus(ws, views)
	return nil
}

// buildWorkspaceServiceStatus derives one service's row from deployment
// reality, never from possibly-stale status strings. Reconcile reloads the
// registry, so the record is read again afterwards; the pre-reconcile pointer
// belongs to a discarded map. A classic service keeps no deploy.json, so the
// reconciled app record is its lifecycle truth.
func buildWorkspaceServiceStatus(svc *workspace.Service) workspaceServiceStatusView {
	view := workspaceServiceStatusView{
		Name:      svc.Name,
		Path:      svc.Path,
		Strategy:  svc.Strategy,
		DependsOn: svc.DependsOn,
		Port:      svc.Port,
	}
	if findRegisteredApp(svc.Name) == nil {
		view.Status = "not registered"
		return view
	}

	reconcileAppWithDeploy(svc.Name)
	info := findRegisteredApp(svc.Name)
	if info == nil {
		view.Status = "not registered"
		return view
	}
	view.Registered = true

	meta, _ := deploy.CurrentVersionMeta(svc.Name)
	if state, err := loadDeployState(svc.Name); err == nil && state != nil {
		lifecycle := deploy.DecideLifecycle(state)
		view.Status = lifecycle.Status
		if lifecycle.PublicPort > 0 {
			view.Port = lifecycle.PublicPort
		}
	} else if meta != nil {
		view.Status = info.Status
	} else {
		view.Status = "not deployed"
	}
	if view.Port == 0 && info.Port > 0 {
		view.Port = info.Port
	}

	if meta != nil {
		view.Version = meta.Version
		view.Tag = meta.Tag
	}
	view.Health, view.Endpoints = workspaceHealthSummary(info.ID)
	return view
}

// workspaceHealthSummary probes every configured endpoint once with a
// bounded timeout and rolls the results up. No configuration at all yields
// an empty summary, matching the health contract that absence means
// "not configured", never "unhealthy". An unreadable health store degrades
// the column the same way: a broken status view is worse than a missing
// health cell.
func workspaceHealthSummary(appID string) (string, []workspaceEndpointHealthView) {
	configMgr, err := health.InitConfigManager()
	if err != nil {
		return "", nil
	}
	hc := configMgr.GetConfig(appID)
	if hc == nil || !hc.Enabled || len(hc.Endpoints) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(hc.Endpoints))
	for name := range hc.Endpoints {
		names = append(names, name)
	}
	sort.Strings(names)

	checker := health.NewChecker()
	views := make([]workspaceEndpointHealthView, 0, len(names))
	healthy := true
	for _, name := range names {
		endpoint := *hc.Endpoints[name]
		endpoint.Timeout = wsStatusProbeTimeout
		result := checker.Check(&endpoint)
		status := "DOWN"
		if result != nil && result.Status != "" {
			status = result.Status
		}
		if status != "UP" {
			healthy = false
		}
		views = append(views, workspaceEndpointHealthView{Name: name, URL: endpoint.URL, Status: status})
	}
	if healthy {
		return "healthy", views
	}
	return "unhealthy", views
}

// renderWorkspaceStatus prints the human overview table.
func renderWorkspaceStatus(ws *workspace.Workspace, views []workspaceServiceStatusView) {
	printWorkspaceHeader(ws, "status")
	table := tablewriter.NewTable(os.Stdout,
		tablewriter.WithRendition(tw.Rendition{
			Settings: tw.Settings{Separators: tw.Separators{BetweenRows: tw.On}},
		}),
		tablewriter.WithConfig(tablewriter.Config{
			Header: tw.CellConfig{
				Alignment: tw.CellAlignment{Global: tw.AlignCenter},
			},
			Row: tw.CellConfig{
				Alignment: tw.CellAlignment{
					Global:    tw.AlignCenter,
					PerColumn: []tw.Align{tw.AlignLeft},
				},
			},
		}),
	)
	table.Header([]string{"Name", "Status", "Version", "Health", "Port", "Strategy", "Depends On"})
	for _, view := range views {
		version := "-"
		if view.Version > 0 {
			version = fmt.Sprintf("v%d", view.Version)
		}
		healthCol := view.Health
		if healthCol == "" {
			healthCol = "-"
		}
		port := "-"
		if view.Port > 0 {
			port = fmt.Sprintf("%d", view.Port)
		}
		table.Append([]string{
			color.CyanString(view.Name),
			colorWorkspaceStatus(view.Status),
			version,
			colorWorkspaceHealth(healthCol),
			port,
			workspaceStrategyColumn(view.Strategy),
			fmt.Sprintf("%v", view.DependsOn),
		})
	}
	_ = table.Render()
}

// colorWorkspaceStatus colors the derived lifecycle status.
func colorWorkspaceStatus(status string) string {
	switch status {
	case "running":
		return color.GreenString(status)
	case "degraded":
		return color.YellowString(status)
	case "stopped":
		return color.RedString(status)
	default:
		return color.New(color.Faint).Sprint(status)
	}
}

// colorWorkspaceHealth colors the rolled-up health summary.
func colorWorkspaceHealth(healthCol string) string {
	switch healthCol {
	case "healthy":
		return color.GreenString(healthCol)
	case "unhealthy":
		return color.RedString(healthCol)
	default:
		return color.New(color.Faint).Sprint(healthCol)
	}
}

// workspaceStrategyColumn renders the declared strategy, defaulting the
// display to classic when nothing is declared anywhere.
func workspaceStrategyColumn(strategy string) string {
	if strategy == "" {
		return "classic"
	}
	return strategy
}
