package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/fatih/color"
)

// This file integrates Build Reports with the CLI:
//
//   - newNativeBuildReport assembles the report captured for one native
//     Go/Rust build (cheap: timestamps around ExecuteBuild + one artifact
//     stat; no recompilation, no execution of the app).
//   - emitBuildReport prints the post-build report plus regression analysis.
//     Reporting is observability: any failure degrades to a warning and can
//     never turn a successful build into a failed command.
//   - printFailedBuildSummary renders the failed-build post-mortem (failed
//     builds are never recorded as versions).

// newNativeBuildReport assembles a buildreport.Report from data already
// available in the build context. binaryPath is stat-ed once for artifact
// size; every other value comes from existing plumbing (timestamps set by
// ExecuteBuild, cache/toolchain observations collected by the builders).
func newNativeBuildReport(lang builder.Language, cfg *builder.BuildConfig, binaryPath string) *buildreport.Report {
	rep := &buildreport.Report{
		Language: string(lang),
		Cache:    buildreport.CacheInfo{Status: buildreport.CacheUnknown},
		Artifact: buildreport.ArtifactInfo{
			Type:     buildreport.ArtifactBinary,
			Platform: fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		},
	}
	if lang == builder.Rust {
		rep.Compiler = "rust/cargo"
	} else {
		rep.Compiler = "go"
	}
	if len(cfg.ExtraArgs) > 0 {
		rep.BuildArgs = append([]string(nil), cfg.ExtraArgs...)
	}
	if cfg.Observe != nil {
		rep.CompilerVersion = cfg.Observe.CompilerVersion
		rep.Cache.Status = cfg.Observe.CacheStatus.Normalized()
		rep.Cache.Source = cfg.Observe.CacheSource
	}
	if !cfg.BuildStartTime.IsZero() && !cfg.BuildEndTime.IsZero() {
		rep.StartedAt = cfg.BuildStartTime.UTC()
		rep.EndedAt = cfg.BuildEndTime.UTC()
		rep.DurationMS = cfg.BuildEndTime.Sub(cfg.BuildStartTime).Milliseconds()
	}
	if info, err := os.Stat(binaryPath); err == nil {
		rep.Artifact.SizeBytes = info.Size()
	}
	return rep
}

// emitBuildReport prints the Build Report for a freshly recorded version and,
// when metadata is available, runs the regression analysis against previous
// comparable builds. Never returns an error and never alters the caller's
// exit code path.
func emitBuildReport(appName string, rep *buildreport.Report, commit string, rec *deploy.RecordResult, verErr error) {
	if rep == nil {
		return
	}
	if rec != nil && verErr == nil {
		history, herr := deploy.BuildReportHistory(appName, rec.Version)
		if herr != nil {
			buildreport.PrintReport(os.Stdout, buildreport.ReportView{
				AppName: appName,
				Version: rec.Version,
				Commit:  commit,
				Report:  rep,
			})
			fmt.Printf("  %s Build report warning:\n", color.YellowString("⚠"))
			fmt.Printf("  Unable to compare with previous build: %v\n", herr)
			return
		}
		analysis := buildreport.Analyze(rep, history, buildreport.DefaultConfig())
		buildreport.PrintReport(os.Stdout, buildreport.ReportView{
			AppName:  appName,
			Version:  rec.Version,
			Commit:   commit,
			Report:   rep,
			Analysis: analysis,
		})
		return
	}

	// Version recording failed — the build itself succeeded, so show the
	// raw report with an explanatory warning instead of analysis.
	buildreport.PrintReport(os.Stdout, buildreport.ReportView{
		AppName: appName,
		Commit:  commit,
		Report:  rep,
	})
	fmt.Printf("  %s Build report warning:\n", color.YellowString("⚠"))
	reason := "version metadata unavailable"
	if verErr != nil {
		reason = "unable to compare with previous build: " + verErr.Error()
	}
	fmt.Printf("  %s\n", reason)
}

// printFailedBuildSummary renders concise post-mortem metrics after a failed
// build. cfg may be nil (failure before any timing started). Nothing here is
// persisted: failed builds must not become versions.
func printFailedBuildSummary(lang builder.Language, cfg *builder.BuildConfig) {
	view := buildreport.FailedReportView{Language: string(lang)}
	if cfg != nil {
		if !cfg.BuildEndTime.IsZero() && !cfg.BuildStartTime.IsZero() {
			view.DurationMS = cfg.BuildEndTime.Sub(cfg.BuildStartTime).Milliseconds()
		}
		view.Stage = "compile"
		if cfg.Observe != nil {
			view.CompilerVersion = cfg.Observe.CompilerVersion
			view.Cache = cfg.Observe.CacheStatus.Normalized()
		}
	}
	buildreport.PrintFailedReport(os.Stdout, view)
}
