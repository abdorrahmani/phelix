package deploy

import (
	"context"
	"fmt"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// BuildSource supplies the binary and encrypted-env snapshot for one deploy
// cycle. Blue-green and rolling deploy both call BuildSource instead of
// compiling inline so forward deploys and rollbacks share the same downstream
// path (start inactive slot → health → proxy switch → graceful stop).
//
// Rollback deliberately does NOT flip the "current" symlink or restart via a
// shortcut: ExistingVersionSource only resolves paths to builds/vN and
// env/vN.enc, and the normal Deploy() flow performs the zero-downtime swap.
type BuildSource interface {
	Build(ctx context.Context) (binaryPath, envPath string, err error)
	Describe() string
}

// VersionAwareSource is implemented by sources that know which version label
// will become active after a successful deploy (fresh build or rollback target).
type VersionAwareSource interface {
	BuildSource
	TargetVersion() int
}

// BuilderSource adapts the legacy Builder func for tests. It returns an empty envPath.
func BuilderSource(fn Builder) BuildSource {
	if fn == nil {
		return nil
	}
	return &builderSource{fn: fn}
}

type builderSource struct {
	fn Builder
}

func (b *builderSource) Build(ctx context.Context) (string, string, error) {
	p, err := b.fn(ctx, "", "", nil)
	return p, "", err
}

func (b *builderSource) Describe() string { return "build" }

// ExistingVersionSource resolves an already-built version directory for
// rollback. It never compiles and never mutates versions.json — promotion to
// "current" happens only after Deploy succeeds, same as a forward deploy.
type ExistingVersionSource struct {
	AppName string
	Version int // 0 = previous version
}

func (e *ExistingVersionSource) Build(_ context.Context) (string, string, error) {
	ver := e.Version
	if ver == 0 {
		prev, err := PreviousVersion(e.AppName)
		if err != nil {
			return "", "", err
		}
		ver = prev
	}
	bin, env, err := VersionPaths(e.AppName, ver)
	if err != nil {
		return "", "", err
	}
	return bin, env, nil
}

func (e *ExistingVersionSource) Describe() string {
	if e.Version > 0 {
		return fmt.Sprintf("rollback to v%d", e.Version)
	}
	return "rollback to previous version"
}

func (e *ExistingVersionSource) TargetVersion() int {
	if e.Version > 0 {
		return e.Version
	}
	v, err := PreviousVersion(e.AppName)
	if err != nil {
		return 0
	}
	return v
}

// FreshBuildSource compiles (via BuildFn), stores artifacts under
// ~/.phelix/apps/<AppName>/builds/vN and env/vN.enc, and returns those paths.
type FreshBuildSource struct {
	AppName   string
	AppID     string
	ExtraArgs []string
	BuildFn   func(ctx context.Context, appID string, extraArgs []string) (binaryPath string, err error)
	GitCommit string
	Tag       string
	Retention RetentionPolicy
	Logger    Logger

	lastVersion int
}

func (f *FreshBuildSource) Build(ctx context.Context) (string, string, error) {
	if f.BuildFn == nil {
		return "", "", phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: FreshBuildSource has no BuildFn")
	}
	built, err := f.BuildFn(ctx, f.AppID, f.ExtraArgs)
	if err != nil {
		return "", "", err
	}
	rec, err := RecordFreshBuild(f.AppName, f.AppID, built, f.GitCommit, f.Tag, f.Retention, f.Logger)
	if err != nil {
		return "", "", err
	}
	f.lastVersion = rec.Version
	return rec.BinaryPath, rec.EnvPath, nil
}

func (f *FreshBuildSource) Describe() string {
	if f.lastVersion > 0 {
		return fmt.Sprintf("fresh build v%d", f.lastVersion)
	}
	return "fresh build"
}

func (f *FreshBuildSource) TargetVersion() int { return f.lastVersion }

// targetVersionFromSource reads the version that should become active after deploy.
func targetVersionFromSource(src BuildSource) int {
	if va, ok := src.(VersionAwareSource); ok {
		return va.TargetVersion()
	}
	return 0
}
