package project

import (
	"encoding/json"
	"testing"
)

func TestSchemaJSONMapsConfiguration(t *testing.T) {
	raw, err := SchemaJSON()
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v", doc["$schema"])
	}

	properties := doc["properties"].(map[string]any)
	for _, key := range []string{"name", "port", "watching", "resources", "health", "deploy", "matrix", "webhook"} {
		if _, ok := properties[key]; !ok {
			t.Errorf("top-level property %q missing from the schema", key)
		}
	}
	if _, ok := doc["additionalProperties"]; ok {
		t.Error("additionalProperties must stay open: phelix.yaml ignores unknown keys")
	}

	watching := properties["watching"].(map[string]any)
	if len(watching["enum"].([]any)) != 2 {
		t.Errorf("watching enum = %v, want enable and disable", watching["enum"])
	}
	endpoints := properties["health"].(map[string]any)["properties"].(map[string]any)["endpoints"].(map[string]any)
	item := endpoints["items"].(map[string]any)
	required := item["required"].([]any)
	if len(required) != 2 || required[0] != "name" || required[1] != "path" {
		t.Errorf("health endpoint required = %v, want name and path", required)
	}

	webhook := properties["webhook"].(map[string]any)
	dependent, ok := webhook["dependentRequired"].(map[string]any)
	if !ok {
		t.Fatal("webhook must declare the enabled -> branch/secret_env dependency")
	}
	if len(dependent["enabled"].([]any)) != 2 {
		t.Errorf("enabled dependency = %v, want branch and secret_env", dependent["enabled"])
	}
}

func TestSchemaJSONIsDeterministic(t *testing.T) {
	first, err := SchemaJSON()
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := SchemaJSON()
		if err != nil {
			t.Fatalf("SchemaJSON: %v", err)
		}
		if string(next) != string(first) {
			t.Fatal("SchemaJSON must be byte-for-byte reproducible")
		}
	}
}
