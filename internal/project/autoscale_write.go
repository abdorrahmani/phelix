package project

import (
	"bytes"
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

// SetAutoscalingEnabled writes deploy.autoscaling.enabled into dir/phelix.yaml
// so `phelix autoscale enable/disable` converges with the file the controller
// resolves on every evaluation. The document is edited as a yaml.Node: only
// the enabled value is replaced or appended, and every other key, comment and
// the file's formatting survive, exactly like SetWatching. A missing file is
// CodeNotFound and never created; a malformed file is an error and never
// modified.
func SetAutoscalingEnabled(dir string, enabled bool) error {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return phelixerr.Newf(phelixerr.CodeNotFound, "%s not found in %s", FileName, dir)
		}
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
	}

	var doc yaml.Node
	if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, uerr, "%s is malformed; not modifying it", path)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "%s is malformed: top level is not a mapping; not modifying it", path)
	}
	root := doc.Content[0]

	deployNode := mappingValue(root, "deploy")
	if deployNode == nil || deployNode.Kind != yaml.MappingNode {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"%s has no deploy mapping; add a deploy block before toggling autoscaling", path)
	}
	autoNode := mappingValue(deployNode, "autoscaling")
	if autoNode == nil || autoNode.Kind != yaml.MappingNode {
		autoNode = &yaml.Node{Kind: yaml.MappingNode}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "autoscaling"}
		deployNode.Content = append(deployNode.Content, key, autoNode)
	}

	value := "false"
	if enabled {
		value = "true"
	}
	newValue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: value}
	replaced := false
	for i := 0; i+1 < len(autoNode.Content); i += 2 {
		if autoNode.Content[i].Kind == yaml.ScalarNode && autoNode.Content[i].Value == "enabled" {
			*autoNode.Content[i+1] = *newValue
			replaced = true
			break
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"}
		autoNode.Content = append(autoNode.Content, key, newValue)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if eerr := enc.Encode(&doc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", eerr)
	}
	if cerr := enc.Close(); cerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", cerr)
	}
	if werr := os.WriteFile(path, []byte(buf.String()), 0o644); werr != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, werr, "failed to write %s", path)
	}
	return nil
}

// mappingValue returns the value node for the given key in a mapping node,
// or nil when the key is absent.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Kind == yaml.ScalarNode && mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
