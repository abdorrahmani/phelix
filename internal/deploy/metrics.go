package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type RuntimeMetrics struct {
	ContainerID  string
	CPUPercent   float64
	MemoryBytes  uint64
	MemoryLimit  uint64
	NetworkRX    uint64
	NetworkTX    uint64
	BlockRead    uint64
	BlockWrite   uint64
	State        string
	RestartCount uint32
	Health       string
	ExitCode     int
}

func ContainerMetrics(ctx context.Context, ids ...string) ([]RuntimeMetrics, error) {
	return containerMetrics(ctx, execDockerRunner, ids...)
}

func containerMetrics(ctx context.Context, run dockerRunner, ids ...string) ([]RuntimeMetrics, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	for _, id := range ids {
		owned, err := ownedDockerContainer(ctx, run, id)
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, fmt.Errorf("container ownership verification failed")
		}
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, ids...)
	output, err := run(ctx, args...)
	if err != nil {
		return nil, err
	}
	stats := map[string]RuntimeMetrics{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var sample struct {
			ID        string
			Container string
			CPUPerc   string
			MemUsage  string
			NetIO     string
			BlockIO   string
		}
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			return nil, fmt.Errorf("invalid Docker stats response")
		}
		id := sample.Container
		if id == "" {
			id = sample.ID
		}
		cpu, err := strconv.ParseFloat(strings.TrimSuffix(sample.CPUPerc, "%"), 64)
		if err != nil || cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
			return nil, fmt.Errorf("invalid Docker CPU metric")
		}
		memory, _, err := metricPair(sample.MemUsage)
		if err != nil {
			return nil, err
		}
		rx, tx, err := metricPair(sample.NetIO)
		if err != nil {
			return nil, err
		}
		read, write, err := metricPair(sample.BlockIO)
		if err != nil {
			return nil, err
		}
		stats[id] = RuntimeMetrics{CPUPercent: cpu, MemoryBytes: memory, NetworkRX: rx, NetworkTX: tx, BlockRead: read, BlockWrite: write}
	}
	output, err = run(ctx, append([]string{"inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var objects []struct {
		ID           string
		RestartCount uint32
		HostConfig   struct{ Memory uint64 }
		State        struct {
			Status   string
			ExitCode int
			Health   *struct{ Status string }
		}
	}
	if err := json.Unmarshal([]byte(output), &objects); err != nil {
		return nil, fmt.Errorf("invalid Docker runtime metadata")
	}
	var result []RuntimeMetrics
	for _, object := range objects {
		var sample RuntimeMetrics
		found := false
		for key, value := range stats {
			if key == object.ID || strings.HasPrefix(object.ID, key) {
				sample = value
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("Docker stats missing requested container")
		}
		sample.ContainerID = object.ID
		sample.MemoryLimit = object.HostConfig.Memory
		sample.State = object.State.Status
		sample.ExitCode = object.State.ExitCode
		sample.RestartCount = object.RestartCount
		if object.State.Health != nil {
			sample.Health = object.State.Health.Status
		}
		result = append(result, sample)
	}
	return result, nil
}

func metricPair(value string) (uint64, uint64, error) {
	left, right, ok := strings.Cut(value, "/")
	if !ok {
		return 0, 0, fmt.Errorf("invalid Docker byte metric")
	}
	a, err := metricBytes(left)
	if err != nil {
		return 0, 0, err
	}
	b, err := metricBytes(right)
	return a, b, err
}

func metricBytes(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	units := []struct {
		name   string
		factor float64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1}}
	for _, unit := range units {
		if strings.HasSuffix(value, unit.name) {
			number, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, unit.name)), 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number*unit.factor >= 18446744073709551616 {
				return 0, fmt.Errorf("invalid Docker byte metric")
			}
			return uint64(number * unit.factor), nil
		}
	}
	return 0, fmt.Errorf("unknown Docker byte unit")
}
