package project

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

func validateRemoteYAML(raw []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "phelix.yaml is malformed", err)
	}
	if err := rejectAdditionalDocuments(decoder, FileName); err != nil {
		return err
	}
	nodes := 0
	return checkRemoteYAML(&doc, "", 0, &nodes)
}

func checkRemoteYAML(node *yaml.Node, path string, depth int, nodes *int) error {
	*nodes += 1
	fail := func(message string) error {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "phelix.yaml:%d:%d: %s: %s", node.Line, node.Column, path, message)
	}
	if depth > 100 || *nodes > 100000 {
		return fail("document is too deeply nested or complex")
	}
	switch node.Tag {
	case "", "!!str", "!!null", "!!bool", "!!int", "!!float", "!!timestamp", "!!binary", "!!map", "!!seq", "!!merge":
	default:
		return fail("custom YAML tags are not supported")
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				return fail("mapping keys must be scalar values")
			}
			if seen[key.Value] {
				return phelixerr.Newf(phelixerr.CodeConfiguration, "phelix.yaml:%d:%d: %s: duplicate key %q", key.Line, key.Column, path, key.Value)
			}
			seen[key.Value] = true
			child := key.Value
			if path != "" {
				child = path + "." + key.Value
			}
			if err := checkRemoteYAML(key, child, depth+1, nodes); err != nil {
				return err
			}
			if err := checkRemoteYAML(value, child, depth+1, nodes); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			if err := checkRemoteYAML(child, fmt.Sprintf("%s[%d]", path, i), depth+1, nodes); err != nil {
				return err
			}
		}
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := checkRemoteYAML(child, path, depth, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateRemoteContent checks configuration rules and local reporting consent.
func ValidateRemoteContent(dir string, raw []byte, policy ConfigAccessPolicy) (*Config, error) {
	cfg, err := ValidateContent(dir, raw)
	if err != nil {
		return nil, err
	}
	current, err := ReadContent(dir)
	if err != nil {
		return nil, err
	}
	if err := CheckConfigReadAccess(current.Content, policy.ReadDeniedPaths); err != nil {
		return nil, err
	}
	baseline := current.Content
	if !current.Exists {
		baseline = policy.InitialContent
	}
	if err := checkConfigWriteAccess(baseline, raw, policy.WriteDeniedPaths); err != nil {
		return nil, err
	}
	return cfg, validateRemotePolicy(current.Content, cfg)
}

func validateRemotePolicy(current []byte, cfg *Config) error {
	reporting := cfg.Security != nil && cfg.Security.Vulnerability != nil && cfg.Security.Vulnerability.Reporting
	var consent struct {
		Security *SecurityConfig `yaml:"security"`
	}
	previous := false
	if yaml.Unmarshal(current, &consent) == nil && consent.Security != nil && consent.Security.Vulnerability != nil {
		previous = consent.Security.Vulnerability.Reporting
	}
	if reporting != previous {
		return phelixerr.New(phelixerr.CodePermissionDenied, "security.vulnerability.reporting: reporting consent must be changed locally on the application server")
	}
	return nil
}
