package project

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// SchemaJSON renders the JSON Schema (draft 2020-12) that describes
// phelix.yaml. Property names and nesting come from the yaml tags of the
// configuration structs, so a renamed field cannot drift from the schema, and
// the enum, range and presence hints reuse the same tables validate consults.
// The document deliberately leaves additionalProperties open: the loader
// ignores unknown keys, and the schema must never reject a file Phelix accepts.
// It carries no $id: the file ships with the repository and an editor resolves
// it from the local path.
func SchemaJSON() ([]byte, error) {
	root := configSchemaFor(reflect.TypeFor[Config](), "")
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["title"] = "phelix.yaml"
	root["description"] = "Project configuration for Phelix. Every field is optional: commands resolve missing values from flags, defaults or interactive prompts."
	doc, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode the phelix.yaml JSON Schema", err)
	}
	return append(doc, '\n'), nil
}

// configSchemaFor maps one Go type onto its JSON Schema node. path is the
// dotted yaml location of the type, with "[]" marking array items, and selects
// the annotations that mirror validate.
func configSchemaFor(t reflect.Type, path string) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var schema map[string]any
	switch t {
	case reflect.TypeFor[MatrixRule]():
		schema = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	case reflect.TypeFor[Percent]():
		schema = map[string]any{"type": []string{"integer", "string"}}
	case reflect.TypeFor[MatrixVersion]():
		schema = map[string]any{"type": []string{"string", "number"}}
	default:
		switch t.Kind() {
		case reflect.Struct:
			return annotateConfigSchema(path, configObjectSchema(t, path))
		case reflect.Slice:
			schema = map[string]any{"type": "array", "items": configSchemaFor(t.Elem(), path+"[]")}
		case reflect.String:
			schema = map[string]any{"type": "string"}
		case reflect.Bool:
			schema = map[string]any{"type": "boolean"}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			schema = map[string]any{"type": "integer"}
		case reflect.Float32, reflect.Float64:
			schema = map[string]any{"type": "number"}
		default:
			schema = map[string]any{}
		}
	}
	return annotateConfigSchema(path, schema)
}

// configObjectSchema builds the object node for a struct: one property per
// exported field carrying a yaml name, plus the required and dependency rules
// validate enforces at this location.
func configObjectSchema(t reflect.Type, path string) map[string]any {
	properties := make(map[string]any, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		child := name
		if path != "" {
			child = path + "." + name
		}
		properties[name] = configSchemaFor(field.Type, child)
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if required, ok := configSchemaRequired[path]; ok {
		schema["required"] = required
	}
	if dependent, ok := configSchemaDependentRequired[path]; ok {
		schema["dependentRequired"] = dependent
	}
	return schema
}

// annotateConfigSchema merges the hints and descriptions registered for path
// into schema.
func annotateConfigSchema(path string, schema map[string]any) map[string]any {
	for key, value := range configSchemaAnnotations[path] {
		schema[key] = value
	}
	if description, ok := configSchemaDescriptions[path]; ok {
		schema["description"] = description
	}
	return schema
}

// sortedSetKeys returns the keys of a validated-value set in a stable order,
// so the generated document is byte-for-byte reproducible.
func sortedSetKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var configSchemaRequired = map[string][]string{
	"health.endpoints[]":     {"name", "path"},
	"deploy.rollout.steps[]": {"traffic"},
}

var configSchemaDependentRequired = map[string]map[string][]string{
	"webhook": {"enabled": {"branch", "secret_env"}},
}

// configSchemaAnnotations holds the value hints validate enforces. The enums
// are read from the same sets validate consults, so a new strategy or runtime
// appears here without a second edit.
var configSchemaAnnotations = map[string]map[string]any{
	"type":                    {"enum": sortedSetKeys(supportedTypes)},
	"port":                    {"minimum": 1, "maximum": 65535},
	"watching":                {"enum": []string{WatchingEnable, WatchingDisable}},
	"deploy.strategy":         {"enum": sortedSetKeys(supportedStrategies)},
	"deploy.runtime":          {"enum": sortedSetKeys(supportedRuntimes)},
	"deploy.docker.build":     {"enum": sortedSetKeys(supportedDockerBuilds)},
	"health.mode":             {"enum": sortedSetKeys(supportedWorkerHealthModes)},
	"health.exec.command":     {"minItems": 1},
	"health.endpoints[].mode": {"enum": sortedSetKeys(supportedHealthModes)},
	"health.endpoints[].path": {"pattern": "^/"},
	"deploy.rollout.canary":   {"minimum": 1, "maximum": 99},
	"deploy.rollout.steps[].traffic": {
		"minimum": 1, "maximum": 100,
	},
	"process.restart.policy":         {"enum": sortedSetKeys(supportedRestartPolicies)},
	"process.shutdown.signal":        {"enum": sortedSetKeys(supportedShutdownSignals)},
	"deploy.rolling.max_surge":       {"minimum": 1, "maximum": 1},
	"deploy.rolling.max_unavailable": {"minimum": 0, "maximum": 0},
	"secrets.imports[].secret":       {"pattern": secretNamePattern.String()},
	"secrets.imports[].env":          {"pattern": envNamePattern.String()},
}

// configSchemaDescriptions is the hover text an editor shows for each key. It
// documents intent the field names cannot carry (defaults, which values are
// strings instead of numbers, and what a block only applies to).
var configSchemaDescriptions = map[string]string{
	"name":                                               "Application name used by build, rebuild, health and rollback. Optional: build prompts for it when it is missing.",
	"type":                                               "Workload type: service (default, a port-listening app) or worker (a supervised background process with no public port).",
	"process":                                            "Worker supervision policy: restarts, backoff, shutdown. Only valid with type: worker.",
	"process.restart":                                    "Crash handling for worker replicas.",
	"process.restart.policy":                             "always restarts on any exit, on-failure (default) restarts only on a non-zero exit or signal, never leaves a dead replica down.",
	"process.restart.max_attempts":                       "Consecutive failure-driven restarts before the replica enters crashloop. Defaults to 10.",
	"process.restart.backoff":                            "Restart delay: initial * multiplier^attempt, capped at max, plus jitter.",
	"process.restart.backoff.initial":                    "First restart delay as a duration. Defaults to 2s.",
	"process.restart.backoff.max":                        "Restart delay cap as a duration. Defaults to 60s.",
	"process.restart.backoff.multiplier":                 "Growth factor per consecutive attempt. Defaults to 2.",
	"process.shutdown":                                   "Graceful shutdown for worker replicas.",
	"process.shutdown.signal":                            "Signal sent on stop: SIGTERM (default) or SIGINT.",
	"process.shutdown.grace_period":                      "Wait between the signal and SIGKILL, as a duration. Defaults to 30s.",
	"process.startup_timeout":                            "Window a fresh replica has to come up (alive plus health) as a duration. Defaults to 30s.",
	"health.mode":                                        "Worker health mode: process (default, identity liveness) or exec. Only valid with type: worker.",
	"health.exec":                                        "Exec probe run per replica when health.mode is exec.",
	"health.exec.command":                                "Probe argv, run without a shell from the app directory. Exit 0 means healthy.",
	"health.exec.interval":                               "Probe cadence as a duration. Defaults to 10s.",
	"health.exec.timeout":                                "Per-probe timeout as a duration. Defaults to 5s.",
	"health.exec.retries":                                "Consecutive probe failures that mark a replica unhealthy. Defaults to 3.",
	"port":                                               "Public application port. Defaults to 8080.",
	"watching":                                           "Backend monitoring opt-in: enable or disable. When absent the app keeps its persisted flag.",
	"resources":                                          "Per-instance CPU and memory limits, enforced through cgroups v2 on Linux only.",
	"resources.cpu":                                      "CPU ceiling in millicores (500m) or cores (0.5).",
	"resources.memory":                                   "Memory ceiling with binary units (512Mi, 1Gi).",
	"health":                                             "Health endpoints applied to the app on every build or rebuild.",
	"health.endpoints":                                   "Endpoints checked by the health daemon and by deploys.",
	"health.endpoints[].name":                            "Endpoint name. Must be unique within the file.",
	"health.endpoints[].path":                            "HTTP path on localhost. Must start with /.",
	"health.endpoints[].mode":                            "Deploy health tier: auto (default), http, tcp-only or none.",
	"health.endpoints[].codes":                           "Expected HTTP statuses: a range (200-299) or a list (200,204).",
	"health.endpoints[].timeout":                         "Per-request timeout as a duration (5s). Defaults to 10s.",
	"health.replace":                                     "True drops endpoints configured outside this file instead of merging with them.",
	"deploy":                                             "Deployment strategy and runtime.",
	"deploy.strategy":                                    "classic (default), blue-green, rolling, canary or progressive.",
	"deploy.replicas":                                    "Replica count. Only valid with strategy: rolling.",
	"deploy.runtime":                                     "native (default) runs host processes; docker runs each instance as a container, which requires a zero-downtime strategy.",
	"deploy.network":                                     "Docker network attached to each app container. Only valid with runtime: docker.",
	"deploy.docker":                                      "Where the image comes from under runtime: docker.",
	"deploy.docker.build":                                "dockerfile (default) generates or reuses a Dockerfile; compose builds a named compose service.",
	"deploy.docker.compose_file":                         "Compose file to build from, resolved against the build directory. Defaults to docker-compose.yml.",
	"deploy.docker.service":                              "Compose service that builds this app's image. Required when build is compose.",
	"deploy.rollout":                                     "Canary and progressive rollout settings.",
	"deploy.rollout.canary":                              "Canary traffic share as a whole percentage, written as 5 or \"5%\".",
	"deploy.rollout.duration":                            "Verification window before promotion, as a duration (2m).",
	"deploy.rollout.steps":                               "Progressive steps. Shares must strictly increase and end at 100.",
	"deploy.rollout.steps[].traffic":                     "Canary share for this step as a whole percentage.",
	"deploy.rollout.steps[].duration":                    "How long the share is observed, as a duration.",
	"deploy.rollout.verification":                        "Regression bounds compared against the stable version.",
	"deploy.rollout.verification.interval":               "Health and metrics poll cadence as a duration.",
	"deploy.rollout.verification.max_error_rate":         "Absolute canary error-rate cap, in percent.",
	"deploy.rollout.verification.max_error_delta":        "Percentage points the canary error rate may exceed the baseline.",
	"deploy.rollout.verification.max_p95_factor":         "Canary p95 latency ceiling relative to the baseline, at least 1.",
	"deploy.autoscaling":                                 "Replica autoscaling for rolling services and workers. The monitor controller evaluates metrics and scales via the existing scaling paths.",
	"deploy.autoscaling.min_replicas":                    "Lower replica bound. Defaults to 1.",
	"deploy.autoscaling.max_replicas":                    "Upper replica bound. Defaults to deploy.replicas.",
	"deploy.autoscaling.interval":                        "Evaluation cadence as a duration. Defaults to 15s.",
	"deploy.autoscaling.cooldown":                        "Wait between scaling decisions as a duration. Defaults to 60s. Shorthand for both directions.",
	"deploy.autoscaling.evaluation_windows":              "Consecutive samples a condition must hold. Defaults to 1. Shorthand for both directions.",
	"deploy.autoscaling.cpu.scale_up":                    "CPU percent that triggers a scale up. Defaults to 70.",
	"deploy.autoscaling.cpu.scale_down":                  "CPU percent that triggers a scale down. Defaults to 30.",
	"deploy.autoscaling.latency.scale_up_p95":            "Proxy p95 latency that triggers a scale up. Defaults to 500ms.",
	"deploy.autoscaling.latency.scale_down_p95":          "Proxy p95 latency that triggers a scale down. Defaults to 150ms.",
	"deploy.autoscaling.metrics":                         "Per-metric thresholds. Overrides the legacy top-level cpu/latency fields when declared.",
	"deploy.autoscaling.metrics.cpu":                     "CPU percent thresholds (scale_up/scale_down).",
	"deploy.autoscaling.metrics.memory":                  "Memory percent thresholds (scale_up/scale_down) relative to host memory.",
	"deploy.autoscaling.metrics.requests_per_second":     "Proxy request-rate thresholds (scale_up/scale_down).",
	"deploy.autoscaling.metrics.latency":                 "Proxy latency thresholds; p95 and optional p99.",
	"deploy.autoscaling.metrics.latency.scale_up_p95":    "Proxy p95 latency that triggers a scale up.",
	"deploy.autoscaling.metrics.latency.scale_down_p95":  "Proxy p95 latency that triggers a scale down.",
	"deploy.autoscaling.metrics.latency.scale_up_p99":    "Proxy p99 latency that triggers a scale up. Optional.",
	"deploy.autoscaling.metrics.latency.scale_down_p99":  "Proxy p99 latency that triggers a scale down. Optional.",
	"deploy.autoscaling.scale_up":                        "Scale-up cadence overrides: evaluation_windows and cooldown.",
	"deploy.autoscaling.scale_up.evaluation_windows":     "Consecutive samples the up condition must hold. Defaults to the top-level value.",
	"deploy.autoscaling.scale_up.cooldown":               "Wait between scale-up decisions as a duration. Defaults to the top-level cooldown.",
	"deploy.autoscaling.scale_down":                      "Scale-down cadence overrides: evaluation_windows, cooldown, stabilization_window.",
	"deploy.autoscaling.scale_down.evaluation_windows":   "Consecutive samples the down condition must hold. Defaults to the top-level value.",
	"deploy.autoscaling.scale_down.cooldown":             "Wait between scale-down decisions as a duration. Defaults to the top-level cooldown.",
	"deploy.autoscaling.scale_down.stabilization_window": "How long the down condition must have held continuously before a scale-down decision, as a duration. Defaults to 0 (off).",
	"deploy.rolling":                                     "Worker rolling-replacement parameters. Only valid with type: worker.",
	"deploy.rolling.max_surge":                           "Replacements started above the desired count. Only 1 is supported (sequential replacement).",
	"deploy.rolling.max_unavailable":                     "Replicas that may be down during replacement. Only 0 is supported.",
	"deploy.rolling.drain_timeout":                       "Wait after SIGTERM when retiring a replaced replica, as a duration. Falls back to process.shutdown.grace_period.",
	"matrix":                                             "Build matrix profile: toolchain versions by platform.",
	"matrix.enabled":                                     "Turns the matrix profile on for builds that name no matrix flags.",
	"matrix.go":                                          "Go toolchain versions. Mutually exclusive with matrix.rust.",
	"matrix.rust":                                        "Rust toolchain versions. Mutually exclusive with matrix.go.",
	"matrix.go.versions[]":                               "Toolchain version, quoted or bare (\"1.27\" or 1.27).",
	"matrix.rust.versions[]":                             "Toolchain version, quoted or bare (\"1.78\" or 1.78).",
	"matrix.platforms[]":                                 "Target in os/arch form (linux/amd64, linux/arm64/v8).",
	"matrix.concurrency":                                 "Maximum parallel builds.",
	"matrix.retries":                                     "Extra attempts per failed combination.",
	"matrix.include[]":                                   "Extra combinations or attributes. Keys are dimensions (go, rust, lang, version, platform, os, arch, variant) or metadata.",
	"matrix.exclude[]":                                   "Combinations to drop. Keys are dimensions only.",
	"webhook":                                            "Git push webhook trigger. Without this block the app never accepts deliveries.",
	"webhook.enabled":                                    "Turns the webhook trigger on for this app.",
	"webhook.branch":                                     "Bare branch name whose pushes trigger a rebuild, not a full ref.",
	"webhook.secret_env":                                 "Name of the environment variable holding the HMAC-SHA256 secret. The secret itself is never stored in phelix.yaml.",
	"secrets":                                            "Local secret management: maps secrets from the app's encrypted secret store onto environment variables. Values are resolved at launch, never stored in phelix.yaml, and are not part of version snapshots or rollbacks.",
	"secrets.imports":                                    "Declarations of which secrets the application consumes. Manage the secrets themselves with `phelix secrets`.",
	"secrets.imports[].secret":                           "Name of the secret in the app's secret store, for example database_url. It must exist in the store when the app starts or deploys.",
	"secrets.imports[].env":                              "Environment variable name the application receives, for example DATABASE_URL. Each name may be declared once; reserved names such as PORT are rejected.",
}
