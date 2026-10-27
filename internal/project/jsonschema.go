package project

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const configSchemaURL = "https://raw.githubusercontent.com/abdorrahmani/phelix/master/schema.json"

// SchemaJSON renders the JSON Schema (draft 2020-12) that describes
// phelix.yaml. Property names and nesting come from the yaml tags of the
// configuration structs, so a renamed field cannot drift from the schema, and
// the enum, range and presence hints reuse the same tables validate consults.
// The document deliberately leaves additionalProperties open: the loader
// ignores unknown keys, and the schema must never reject a file Phelix accepts.
func SchemaJSON() ([]byte, error) {
	root := configSchemaFor(reflect.TypeFor[Config](), "")
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = configSchemaURL
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
	"port":                    {"minimum": 1, "maximum": 65535},
	"watching":                {"enum": []string{WatchingEnable, WatchingDisable}},
	"deploy.strategy":         {"enum": sortedSetKeys(supportedStrategies)},
	"deploy.runtime":          {"enum": sortedSetKeys(supportedRuntimes)},
	"deploy.docker.build":     {"enum": sortedSetKeys(supportedDockerBuilds)},
	"health.endpoints[].mode": {"enum": sortedSetKeys(supportedHealthModes)},
	"health.endpoints[].path": {"pattern": "^/"},
	"deploy.rollout.canary":   {"minimum": 1, "maximum": 99},
	"deploy.rollout.steps[].traffic": {
		"minimum": 1, "maximum": 100,
	},
}

// configSchemaDescriptions is the hover text an editor shows for each key. It
// documents intent the field names cannot carry (defaults, which values are
// strings instead of numbers, and what a block only applies to).
var configSchemaDescriptions = map[string]string{
	"name":                                        "Application name used by build, rebuild, health and rollback. Optional: build prompts for it when it is missing.",
	"port":                                        "Public application port. Defaults to 8080.",
	"watching":                                    "Backend monitoring opt-in: enable or disable. When absent the app keeps its persisted flag.",
	"resources":                                   "Per-instance CPU and memory limits, enforced through cgroups v2 on Linux only.",
	"resources.cpu":                               "CPU ceiling in millicores (500m) or cores (0.5).",
	"resources.memory":                            "Memory ceiling with binary units (512Mi, 1Gi).",
	"health":                                      "Health endpoints applied to the app on every build or rebuild.",
	"health.endpoints":                            "Endpoints checked by the health daemon and by deploys.",
	"health.endpoints[].name":                     "Endpoint name. Must be unique within the file.",
	"health.endpoints[].path":                     "HTTP path on localhost. Must start with /.",
	"health.endpoints[].mode":                     "Deploy health tier: auto (default), http, tcp-only or none.",
	"health.endpoints[].codes":                    "Expected HTTP statuses: a range (200-299) or a list (200,204).",
	"health.endpoints[].timeout":                  "Per-request timeout as a duration (5s). Defaults to 10s.",
	"health.replace":                              "True drops endpoints configured outside this file instead of merging with them.",
	"deploy":                                      "Deployment strategy and runtime.",
	"deploy.strategy":                             "classic (default), blue-green, rolling, canary or progressive.",
	"deploy.replicas":                             "Replica count. Only valid with strategy: rolling.",
	"deploy.runtime":                              "native (default) runs host processes; docker runs each instance as a container, which requires a zero-downtime strategy.",
	"deploy.network":                              "Docker network attached to each app container. Only valid with runtime: docker.",
	"deploy.docker":                               "Where the image comes from under runtime: docker.",
	"deploy.docker.build":                         "dockerfile (default) generates or reuses a Dockerfile; compose builds a named compose service.",
	"deploy.docker.compose_file":                  "Compose file to build from, resolved against the build directory. Defaults to docker-compose.yml.",
	"deploy.docker.service":                       "Compose service that builds this app's image. Required when build is compose.",
	"deploy.rollout":                              "Canary and progressive rollout settings.",
	"deploy.rollout.canary":                       "Canary traffic share as a whole percentage, written as 5 or \"5%\".",
	"deploy.rollout.duration":                     "Verification window before promotion, as a duration (2m).",
	"deploy.rollout.steps":                        "Progressive steps. Shares must strictly increase and end at 100.",
	"deploy.rollout.steps[].traffic":              "Canary share for this step as a whole percentage.",
	"deploy.rollout.steps[].duration":             "How long the share is observed, as a duration.",
	"deploy.rollout.verification":                 "Regression bounds compared against the stable version.",
	"deploy.rollout.verification.interval":        "Health and metrics poll cadence as a duration.",
	"deploy.rollout.verification.max_error_rate":  "Absolute canary error-rate cap, in percent.",
	"deploy.rollout.verification.max_error_delta": "Percentage points the canary error rate may exceed the baseline.",
	"deploy.rollout.verification.max_p95_factor":  "Canary p95 latency ceiling relative to the baseline, at least 1.",
	"deploy.autoscaling":                          "Replica autoscaling. Parsed, validated and resolved, but no component executes scaling decisions yet.",
	"deploy.autoscaling.min_replicas":             "Lower replica bound. Defaults to 1.",
	"deploy.autoscaling.max_replicas":             "Upper replica bound. Defaults to deploy.replicas.",
	"deploy.autoscaling.interval":                 "Evaluation cadence as a duration. Defaults to 15s.",
	"deploy.autoscaling.cooldown":                 "Wait between scaling decisions as a duration. Defaults to 60s.",
	"deploy.autoscaling.evaluation_windows":       "Consecutive samples a condition must hold. Defaults to 1.",
	"deploy.autoscaling.cpu.scale_up":             "CPU percent that triggers a scale up. Defaults to 70.",
	"deploy.autoscaling.cpu.scale_down":           "CPU percent that triggers a scale down. Defaults to 30.",
	"deploy.autoscaling.latency.scale_up_p95":     "Proxy p95 latency that triggers a scale up. Defaults to 500ms.",
	"deploy.autoscaling.latency.scale_down_p95":   "Proxy p95 latency that triggers a scale down. Defaults to 150ms.",
	"matrix":                 "Build matrix profile: toolchain versions by platform.",
	"matrix.enabled":         "Turns the matrix profile on for builds that name no matrix flags.",
	"matrix.go":              "Go toolchain versions. Mutually exclusive with matrix.rust.",
	"matrix.rust":            "Rust toolchain versions. Mutually exclusive with matrix.go.",
	"matrix.go.versions[]":   "Toolchain version, quoted or bare (\"1.27\" or 1.27).",
	"matrix.rust.versions[]": "Toolchain version, quoted or bare (\"1.78\" or 1.78).",
	"matrix.platforms[]":     "Target in os/arch form (linux/amd64, linux/arm64/v8).",
	"matrix.concurrency":     "Maximum parallel builds.",
	"matrix.retries":         "Extra attempts per failed combination.",
	"matrix.include[]":       "Extra combinations or attributes. Keys are dimensions (go, rust, lang, version, platform, os, arch, variant) or metadata.",
	"matrix.exclude[]":       "Combinations to drop. Keys are dimensions only.",
	"webhook":                "Git push webhook trigger. Without this block the app never accepts deliveries.",
	"webhook.enabled":        "Turns the webhook trigger on for this app.",
	"webhook.branch":         "Bare branch name whose pushes trigger a rebuild, not a full ref.",
	"webhook.secret_env":     "Name of the environment variable holding the HMAC-SHA256 secret. The secret itself is never stored in phelix.yaml.",
}
