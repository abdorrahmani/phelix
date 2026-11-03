package project

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"go.yaml.in/yaml/v3"
)

// configKeyIssue is one key the YAML document declares but Config does not
// know. Line is the source line of the key itself.
type configKeyIssue struct {
	Path string
	Line int
}

// warnedUnknownKeys keeps execution warnings to one per key per process, so a
// daemon reloading the same file does not repeat the same warning forever.
var warnedUnknownKeys sync.Map

// decodeConfig decodes one configuration document. strict enables KnownFields
// so unknown keys fail; permissive mode keeps loading the file and reports the
// unknown keys as warnings at the caller.
func decodeConfig(raw []byte, path string, strict bool) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if strict {
		dec.KnownFields(true)
	}

	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		if issues := unknownConfigKeys(raw); len(issues) > 0 {
			return nil, unknownKeysError(path, issues)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "%s is malformed: %v", path, err)
	}
	if err := rejectAdditionalDocuments(dec, path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// rejectAdditionalDocuments turns the decoder's "first document wins" default
// into an explicit error. An empty trailing document is harmless and ignored.
func rejectAdditionalDocuments(dec *yaml.Decoder, path string) error {
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "%s is malformed: %v", path, err)
		}
		if yamlNodeEmpty(&doc) {
			continue
		}
		line := doc.Line
		if line == 0 && len(doc.Content) > 0 {
			line = doc.Content[0].Line
		}
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"%s:%d: multiple YAML documents are not supported\nHint: keep a single phelix.yaml document", path, line)
	}
}

func yamlNodeEmpty(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return true
		}
		return yamlNodeEmpty(node.Content[0])
	}
	return node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null")
}

// unknownConfigKeys walks the first YAML document and reports every mapping
// key that does not name a field of the corresponding struct. It is the
// location-aware companion of KnownFields: yaml.v3 names the type but not the
// dotted key path.
func unknownConfigKeys(raw []byte) []configKeyIssue {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	var issues []configKeyIssue
	collectUnknownKeys(doc.Content[0], reflect.TypeFor[Config](), "", &issues)
	return issues
}

func collectUnknownKeys(node *yaml.Node, t reflect.Type, prefix string, issues *[]configKeyIssue) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if skipUnknownKeyTraversal(t) {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "<<" {
				continue
			}
			field, ok := yamlField(t, key.Value)
			childPath := key.Value
			if prefix != "" {
				childPath = prefix + "." + key.Value
			}
			if !ok {
				*issues = append(*issues, configKeyIssue{Path: childPath, Line: key.Line})
				continue
			}
			collectUnknownKeys(value, field.Type, childPath, issues)
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for _, item := range node.Content {
			collectUnknownKeys(item, t.Elem(), prefix+"[]", issues)
		}
	}
}

func yamlField(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		if tag == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func skipUnknownKeyTraversal(t reflect.Type) bool {
	return t == reflect.TypeFor[MatrixRule]() ||
		t == reflect.TypeFor[MatrixVersion]() ||
		t == reflect.TypeFor[Percent]()
}

func unknownKeysError(path string, issues []configKeyIssue) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s has unknown key(s):", path)
	for _, issue := range issues {
		fmt.Fprintf(&b, "\n  %s:%d: %s", path, issue.Line, issue.Path)
	}
	b.WriteString("\nHint: remove or rename the key; see 'phelix config schema' for the accepted keys")
	return phelixerr.New(phelixerr.CodeConfiguration, b.String())
}

// warnUnknownKeys records the transition behavior for execution: the file
// still loads, but every unknown key is logged as a warning.
func warnUnknownKeys(path string, raw []byte) {
	for _, issue := range unknownConfigKeys(raw) {
		key := fmt.Sprintf("%s:%d:%s", path, issue.Line, issue.Path)
		if _, loaded := warnedUnknownKeys.LoadOrStore(key, true); loaded {
			continue
		}
		logs.Warning("project", "%s:%d: unknown key %q ignored; run 'phelix config lint' to reject unknown keys",
			path, issue.Line, issue.Path)
	}
}
