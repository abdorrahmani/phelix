package project

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

type ConfigDeniedPath struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ConfigAccessPolicy struct {
	ReadDeniedPaths  []ConfigDeniedPath
	WriteDeniedPaths []ConfigDeniedPath
	InitialContent   []byte
}

// CheckConfigReadAccess protects configuration sections with separate permissions.
func CheckConfigReadAccess(raw []byte, denied []ConfigDeniedPath) error {
	if len(denied) == 0 || len(raw) == 0 {
		return nil
	}
	document, err := decodeConfigAccess(raw)
	if err != nil {
		return phelixerr.New(phelixerr.CodePermissionDenied, "configuration cannot be inspected safely with the current permissions")
	}
	for _, rule := range denied {
		if value := configAccessValue(document, strings.Split(rule.Path, ".")); value != nil {
			return configAccessDenied(rule)
		}
	}
	return nil
}

func checkConfigWriteAccess(current, proposed []byte, denied []ConfigDeniedPath) error {
	if len(denied) == 0 || bytes.Equal(current, proposed) {
		return nil
	}
	oldDocument, err := decodeConfigAccess(current)
	if err != nil {
		return phelixerr.New(phelixerr.CodePermissionDenied, "configuration cannot be compared safely with the current permissions; repair it locally first")
	}
	newDocument, err := decodeConfigAccess(proposed)
	if err != nil {
		return err
	}
	for _, rule := range denied {
		oldValue := configAccessValue(oldDocument, strings.Split(rule.Path, "."))
		newValue := configAccessValue(newDocument, strings.Split(rule.Path, "."))
		if rule.Path == "type.job" {
			oldValue = strings.TrimSpace(configAccessString(oldDocument["type"])) == TypeJob
			newValue = strings.TrimSpace(configAccessString(newDocument["type"])) == TypeJob
		}
		if !reflect.DeepEqual(oldValue, newValue) {
			return configAccessDenied(rule)
		}
	}
	return nil
}

func decodeConfigAccess(raw []byte) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	nodes := 0
	value, err := configAccessNode(doc.Content[0], 0, &nodes)
	if err != nil {
		return nil, err
	}
	document, ok := value.(map[string]any)
	if !ok && value != nil {
		return nil, fmt.Errorf("configuration must be a mapping")
	}
	return document, nil
}

func configAccessNode(node *yaml.Node, depth int, nodes *int) (any, error) {
	*nodes += 1
	if depth > 100 || *nodes > 100000 {
		return nil, fmt.Errorf("configuration aliases or nesting exceed the document limit")
	}
	if node.Kind == yaml.AliasNode {
		return configAccessNode(node.Alias, depth+1, nodes)
	}
	switch node.Kind {
	case yaml.MappingNode:
		values := make(map[string]any, len(node.Content)/2)
		seen := make(map[string]bool, len(node.Content)/2)
		var merges []*yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, child := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || seen[key.Value] {
				return nil, fmt.Errorf("configuration mapping keys are ambiguous")
			}
			seen[key.Value] = true
			if key.Tag == "!!merge" {
				merges = append(merges, child)
				continue
			}
			value, err := configAccessNode(child, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			values[key.Value] = value
		}
		for _, merge := range merges {
			value, err := configAccessNode(merge, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			sources := []any{value}
			if list, ok := value.([]any); ok {
				sources = list
			}
			for _, source := range sources {
				object, ok := source.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("configuration merge must contain mappings")
				}
				for key, child := range object {
					if _, exists := values[key]; !exists {
						values[key] = child
					}
				}
			}
		}
		return values, nil
	case yaml.SequenceNode:
		values := make([]any, len(node.Content))
		for i, child := range node.Content {
			value, err := configAccessNode(child, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			values[i] = value
		}
		return values, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!str":
			return node.Value, nil
		case "!!bool":
			var value bool
			err := node.Decode(&value)
			return value, err
		default:
			return struct{ Tag, Value string }{node.Tag, node.Value}, nil
		}
	}
	return nil, nil
}

func configAccessValue(value any, path []string) any {
	if len(path) == 0 {
		return value
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if path[0] != "*" {
		return configAccessValue(object[path[0]], path[1:])
	}
	values := make(map[string]any)
	for key, child := range object {
		if selected := configAccessValue(child, path[1:]); selected != nil {
			values[key] = selected
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func configAccessString(value any) string {
	text, _ := value.(string)
	return text
}

func configAccessDenied(rule ConfigDeniedPath) error {
	return phelixerr.Newf(phelixerr.CodePermissionDenied, "%s: %s", rule.Path, rule.Message)
}
