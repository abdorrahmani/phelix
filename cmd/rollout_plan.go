package cmd

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
)

// rolloutPlanFromProject resolves the engine-ready rollout plan for a
// strategy coming from --strategy or phelix.yaml:
//
//   - canary: a single canary share (deploy.rollout.canary, default 10%) held
//     for deploy.rollout.duration (default 30s), then the 100% promotion.
//   - progressive: the explicit deploy.rollout.steps, which the project
//     package has already validated (increasing, ending at 100%).
//
// Verification thresholds come from deploy.rollout.verification when present;
// every unset field keeps the deploy package's defaults.
func rolloutPlanFromProject(cfg *project.Config, strategy string) (*deploy.RolloutPlan, error) {
	var rc *project.RolloutConfig
	if cfg != nil && cfg.Deploy != nil {
		rc = cfg.Deploy.Rollout
	}

	verification := deploy.VerificationConfig{}
	if rc != nil && rc.Verification != nil {
		if rc.Verification.Interval != "" {
			d, err := time.ParseDuration(rc.Verification.Interval)
			if err != nil || d <= 0 {
				return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
					"invalid deploy.rollout.verification.interval %q: expected a positive duration like 5s", rc.Verification.Interval)
			}
			verification.Interval = d
		}
		verification.MaxErrorRate = rc.Verification.MaxErrorRate
		verification.MaxErrorDelta = rc.Verification.MaxErrorDelta
		verification.MaxP95Factor = rc.Verification.MaxP95Factor
	}

	switch strategy {
	case project.StrategyCanary:
		percent := deploy.DefaultCanaryPercent
		if rc != nil && rc.Canary != nil {
			percent = int(*rc.Canary)
		}
		duration := deploy.DefaultCanaryVerification
		if rc != nil && rc.Duration != "" {
			d, err := time.ParseDuration(rc.Duration)
			if err != nil || d < 0 {
				return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
					"invalid deploy.rollout.duration %q: expected a duration like 2m (or 0s to verify and promote immediately)", rc.Duration)
			}
			duration = d
		}
		plan := deploy.CanaryPlan(percent, duration, verification)
		return &plan, nil

	case project.StrategyProgressive:
		if rc == nil || len(rc.Steps) == 0 {
			return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
				"progressive strategy requires rollout steps\nHint: add a deploy.rollout.steps section to %s, e.g.\n"+
					"  deploy:\n    strategy: progressive\n    rollout:\n      steps:\n        - traffic: 5%%\n          duration: 2m\n        - traffic: 100%%",
				project.FileName)
		}
		steps := make([]deploy.RolloutStep, 0, len(rc.Steps))
		for _, s := range rc.Steps {
			d := time.Duration(0)
			if s.Duration != "" {
				parsed, err := time.ParseDuration(s.Duration)
				if err != nil || parsed < 0 {
					return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
						"invalid rollout step duration %q: expected a duration like 2m", s.Duration)
				}
				d = parsed
			}
			steps = append(steps, deploy.RolloutStep{TrafficPercent: int(s.Traffic), Duration: d})
		}
		plan := deploy.RolloutPlan{
			Strategy:     deploy.StrategyProgressive,
			Steps:        steps,
			Verification: verification,
		}
		return &plan, nil
	}
	return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
		"unsupported rollout strategy %q: expected canary or progressive", strategy)
}

// canaryPlanFromFlag builds the one-shot plan behind `--canary N`. The
// verification window and thresholds still come from phelix.yaml when
// configured there, so a project can tune verification once and use the flag
// for the share alone.
func canaryPlanFromFlag(cfg *project.Config, percent int) (*deploy.RolloutPlan, error) {
	plan, err := rolloutPlanFromProject(cfg, project.StrategyCanary)
	if err != nil {
		return nil, err
	}
	plan.Steps[0].TrafficPercent = percent
	return plan, nil
}
