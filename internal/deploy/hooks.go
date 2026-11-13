package deploy

import (
	"context"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

type HookRequest struct {
	Name            string
	Version         int
	PreviousVersion int
}

type LifecycleHooks interface {
	RunHook(ctx context.Context, req HookRequest) error
}

func runHook(ctx context.Context, hooks LifecycleHooks, name string, version, previous int) error {
	if hooks == nil {
		return nil
	}
	if err := hooks.RunHook(ctx, HookRequest{Name: name, Version: version, PreviousVersion: previous}); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeDeployFailed, err, "%s hook failed", name)
	}
	return nil
}

type HookBuildSource struct {
	Inner    BuildSource
	Hooks    LifecycleHooks
	Previous int
}

func (s *HookBuildSource) Build(ctx context.Context) (string, string, error) {
	if s.Inner == nil {
		return "", "", phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: no build source configured")
	}
	if err := runHook(ctx, s.Hooks, "pre_build", 0, s.Previous); err != nil {
		return "", "", err
	}
	binaryPath, envPath, err := s.Inner.Build(ctx)
	if err != nil {
		return "", "", err
	}
	if err := runHook(ctx, s.Hooks, "post_build", targetVersionFromSource(s.Inner), s.Previous); err != nil {
		return "", "", err
	}
	return binaryPath, envPath, nil
}

func (s *HookBuildSource) Describe() string {
	if s.Inner == nil {
		return "build"
	}
	return s.Inner.Describe()
}

func (s *HookBuildSource) TargetVersion() int {
	if s.Inner == nil {
		return 0
	}
	if aware, ok := s.Inner.(VersionAwareSource); ok {
		return aware.TargetVersion()
	}
	return targetVersionFromSource(s.Inner)
}
