# Deployment Rollout Analysis

Phelix measures what a deployment actually did to an application. After a
successful deploy it compares the previous version's baseline with the new
version's observed behavior, then stores a report you can read offline.

Analysis is observability only. It never blocks a deployment, never changes a
rollback decision, and never fails a deploy when monitoring data is missing.

```text
→ Deployment Analysis
  Application   api
  Deployment    5f3c9a2e
  Strategy      rolling
  Versions      v11 to v12
  Outcome       promoted with regressions
  Status        REGRESSION
  Impact score  78.4/100 (grade B)
  Recommendation Deployment promoted with regressions in Error rate and P95 latency. Review the affected metrics and consider a rollback if the impact persists.
  Analyzed      2026-11-12 10:42:17
```

## How it works

Phelix runs the analysis in two windows:

- **Baseline**: the period immediately before the deployment. It is captured
  when the deployment finishes, from the metric history the local monitor
  already recorded.
- **Observed**: the period after the deployment finishes, controlled by
  `observe_window`. The report is finalized once that window elapses and
  enough samples exist.

A report needs a minimum number of samples in each window. When either window
has too few samples, the affected metric is reported as insufficient data
instead of being treated as a zero.

Analysis happens automatically:

- the monitor finalizes due reports on its normal tick;
- the next CLI command that reads reports also finalizes anything due;
- the next deployment of the same app finalizes any previous pending report
  first, so old traffic never leaks into a new baseline.

## What is measured

| Metric | Direction | Default threshold |
|--------|-----------|-------------------|
| Error rate | Lower is better | +1.0 percentage point |
| P95 latency | Lower is better | +20% |
| Health check failures | Lower is better | +0 additional failures |
| Restarts | Lower is better | +0 additional restarts |
| Throughput | Higher is better | -20% |
| CPU average and peak | Lower is better | +25% average |
| Memory average and peak | Lower is better | +15% average |

Every window also records its sample count, so you can judge how much data
backs a comparison.

## Statuses

- **NORMAL**: all measured metrics stayed inside their thresholds.
- **REGRESSION**: at least one measured metric crossed its threshold.
- **IMPROVEMENT**: at least one measured metric improved past its threshold
  and none regressed.
- **INSUFFICIENT_DATA**: the window did not have enough samples to compare.
- **NOT_AVAILABLE**: the metric was not recorded at all.

Missing data is never rendered as a normal zero.

## Deployment Impact Score

The impact score is a 0 to 100 summary of the measured metrics:

- 100 means every measured metric stayed inside its threshold.
- Regressions subtract points, weighted by importance: error rate, P95 latency
  and health failures weigh 3; restarts and throughput weigh 2; CPU and memory
  weigh 1.
- Improvements can add back at most 10 points.
- Metrics reported as unavailable or insufficient data do not contribute at
  all, so missing data can neither inflate nor deflate the score.
- When no metric is measurable, the score is unavailable and the grade is
  `N/A`. It is not reported as 100.

Grades: A (90 and above), B (75 and above), C (60 and above), D (40 and above),
F (below 40).

## Outcomes

Each report separates the deployment outcome from the metric verdict:

- **promoted**: the new version is serving and the measured metrics are normal
  or improved.
- **promoted with regressions**: the new version is serving but one or more
  metrics regressed. Review the recommendation.
- **rolled back**: the deployment failed and Phelix restored the previous
  known-good version.
- **rejected**: the deployment failed before promotion, including a canary or
  progressive rollout that aborted.
- **insufficient data**: the deployment completed but there was not enough
  monitoring data to judge it.

For canary and progressive rollouts the report also records the rollout step
count, the failed step if any, the abort reason, and the verification numbers
the rollout itself used. Analysis does not weaken the existing verification or
rollback rules: a rollout that rejects or rolls back is recorded with that
outcome and its metrics are not used to override it.

## Configuration

Analysis is enabled by default. Add a `deploy.analysis` block to `phelix.yaml`
to change the windows or thresholds:

```yaml
deploy:
  strategy: rolling
  analysis:
    enabled: true
    baseline_window: 5m
    observe_window: 2m
    min_samples: 5
    error_rate_max_delta: 1.0
    latency_max_percent: 20
    cpu_max_percent: 25
    memory_max_percent: 15
    restart_max_extra: 0
    throughput_max_degradation: 20
    health_failures_max_extra: 0
```

Every field is optional. A zero or invalid value falls back to the built-in
default, so an incomplete block is harmless. Set `enabled: false` to stop
recording and analyzing deployments for that application.

Analysis needs monitoring data to be useful. The app must have been watched by
the monitor long enough to fill the baseline window.

## Reading reports

```bash
phelix deployment-report api                # recent analyses
phelix deployment-report api --latest       # most recent summary
phelix deployment-report api --detailed     # most recent full comparison
phelix deployment-report api --version v12  # the analysis for one version
phelix deployment-report api --json         # machine-readable output
```

`--detailed` prints the per-metric table, peak CPU and memory, the sample
counts for both windows, and any reason a metric could not be compared.

Reports are stored locally under the application's data directory and work
offline. The last 100 reports per application are retained. Corrupt storage is
moved aside automatically so the next report starts from a clean file.

When Phelix is connected to a backend account, reports are synchronized
automatically. Reports created while offline stay queued and are delivered on a
later command once a session is available.
