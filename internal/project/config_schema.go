package project

import (
	"encoding/json"
	"reflect"
	"strings"
)

// EditorSchemaJSON describes the fields available in the dashboard builder.
func EditorSchemaJSON() ([]byte, error) { return EditorSchemaJSONForAccess(nil, nil) }

// EditorSchemaJSONForAccess marks settings unavailable to the current account.
func EditorSchemaJSONForAccess(denied []ConfigDeniedPath, current []byte) ([]byte, error) {
	root := configSchemaFor(reflect.TypeFor[Config](), "")
	enrichEditorSchema(reflect.TypeFor[Config](), root, "")
	for _, rule := range denied {
		if rule.Path == "type.job" {
			node := root["properties"].(map[string]any)["type"].(map[string]any)
			document, _ := decodeConfigAccess(current)
			if strings.TrimSpace(configAccessString(document["type"])) == TypeJob {
				node["readOnly"] = true
			} else {
				node["enum"] = []string{TypeService, TypeWorker}
			}
			node["description"] = rule.Message
		} else {
			denyEditorSchemaPath(root, strings.Split(rule.Path, "."), rule.Message)
		}
	}
	root["title"] = "phelix.yaml"
	return json.Marshal(root)
}

func enrichEditorSchema(t reflect.Type, node map[string]any, path string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		properties, ok := node["properties"].(map[string]any)
		if !ok {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if !field.IsExported() || name == "" || name == "-" {
				continue
			}
			child := name
			if path != "" {
				child = path + "." + name
			}
			enrichEditorSchema(field.Type, properties[name].(map[string]any), child)
		}
	case reflect.Slice:
		enrichEditorSchema(t.Elem(), node["items"].(map[string]any), path+"[]")
	case reflect.Map:
		node["type"] = "object"
		node["additionalProperties"] = configSchemaFor(t.Elem(), path+".*")
	}
	switch path {
	case "port", "deploy.rolling.max_surge":
		node["minimum"] = 0
	case "security.vulnerability.fail_threshold":
		node["enum"] = []string{"low", "medium", "high", "critical"}
	case "security.vulnerability.reporting":
		node["readOnly"] = true
		node["description"] = "Security reporting consent can only be changed on the application server."
	}
}

func denyEditorSchemaPath(node map[string]any, path []string, message string) {
	if len(path) == 0 {
		node["readOnly"] = true
		node["description"] = message
		return
	}
	properties, ok := node["properties"].(map[string]any)
	if !ok {
		return
	}
	if path[0] == "*" {
		for _, child := range properties {
			denyEditorSchemaPath(child.(map[string]any), path[1:], message)
		}
	} else if child, ok := properties[path[0]].(map[string]any); ok {
		denyEditorSchemaPath(child, path[1:], message)
	}
}
