// Package resources applies per-instance runtime limits independently of artifacts.
package resources

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

const CPUPeriod uint64 = 100000

type Config struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}

type Limits struct {
	CPUQuota    uint64
	MemoryBytes uint64
}

func (c Config) IsZero() bool { return c.CPU == "" && c.Memory == "" }

func (c Config) Validate() error {
	_, err := c.Limits()
	return err
}

func (c Config) Limits() (Limits, error) {
	var l Limits
	var err error
	if c.CPU != "" {
		l.CPUQuota, err = ParseCPU(c.CPU)
		if err != nil {
			return l, err
		}
	}
	if c.Memory != "" {
		l.MemoryBytes, err = ParseMemory(c.Memory)
	}
	return l, err
}

func (c *Config) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("resources: expected a mapping with cpu and/or memory")
	}
	seen := map[string]bool{}
	var out Config
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		if key != "cpu" && key != "memory" {
			return fmt.Errorf("resources.%s: unknown resource; expected cpu or memory", key)
		}
		if seen[key] {
			return fmt.Errorf("resources.%s: duplicate key", key)
		}
		seen[key] = true
		if value.Kind != yaml.ScalarNode || value.Tag == "!!null" || value.Value == "" {
			return fmt.Errorf("invalid resources.%s: explicitly configured limit must not be empty", key)
		}
		if key == "cpu" {
			out.CPU = value.Value
		} else {
			out.Memory = value.Value
		}
	}
	if err := out.Validate(); err != nil {
		return err
	}
	*c = out
	return nil
}

var cpuPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,3})?$`)
var milliPattern = regexp.MustCompile(`^[1-9][0-9]*m$`)
var memoryPattern = regexp.MustCompile(`^([1-9][0-9]*)(Ki|Mi|Gi|Ti)$`)

// ParseCPU returns cpu.max quota in microseconds for CPUPeriod.
func ParseCPU(s string) (uint64, error) {
	invalid := func(reason string) (uint64, error) { return 0, fmt.Errorf("invalid resources.cpu %q: %s", s, reason) }
	var milli uint64
	var err error
	// The kernel converts cpu.max microseconds to signed nanoseconds.
	const maxQuota = uint64(math.MaxInt64 / 1000)
	if milliPattern.MatchString(s) {
		milli, err = strconv.ParseUint(strings.TrimSuffix(s, "m"), 10, 64)
	} else if cpuPattern.MatchString(s) {
		parts := strings.SplitN(s, ".", 2)
		whole, e := strconv.ParseUint(parts[0], 10, 64)
		if e != nil || whole > maxQuota/CPUPeriod {
			return invalid("quantity overflows cpu.max")
		}
		milli = whole * 1000
		if len(parts) == 2 {
			fraction, _ := strconv.ParseUint(parts[1]+strings.Repeat("0", 3-len(parts[1])), 10, 64)
			milli += fraction
		}
	} else {
		return invalid("expected CPU quantity such as 500m or 2 (at most three decimal places)")
	}
	if err != nil || milli > maxQuota/100 {
		return invalid("quantity overflows cpu.max")
	}
	if milli < 10 {
		return invalid("cpu.max requires at least 10m with a 100000 microsecond period")
	}
	return milli * 100, nil
}

func ParseMemory(s string) (uint64, error) {
	m := memoryPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid resources.memory %q: expected positive integer with binary unit Ki, Mi, Gi or Ti; unsupported units are not accepted", s)
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	shift := map[string]uint{"Ki": 10, "Mi": 20, "Gi": 30, "Ti": 40}[m[2]]
	if err != nil || n > uint64(math.MaxInt64-65535)>>shift {
		return 0, fmt.Errorf("invalid resources.memory %q: quantity overflows memory.max", s)
	}
	return n << shift, nil
}
