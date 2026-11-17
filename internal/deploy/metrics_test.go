package deploy

import (
	"context"
	"errors"
	"testing"
)

func TestDockerRuntimeMetrics(t *testing.T) {
	run := func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "stats" {
			return `{"Container":"abc","CPUPerc":"12.5%","MemUsage":"2MiB / 1GiB","NetIO":"3kB / 4kB","BlockIO":"5B / 6B"}`, nil
		}
		return `[{"Id":"abcdef","RestartCount":2,"HostConfig":{"Memory":1048576},"State":{"Status":"running","ExitCode":0,"Health":{"Status":"healthy"}}}]`, nil
	}
	values, err := containerMetrics(context.Background(), run, "abcdef")
	if err != nil || len(values) != 1 {
		t.Fatal(values, err)
	}
	m := values[0]
	if m.CPUPercent != 12.5 || m.MemoryBytes != 2<<20 || m.MemoryLimit != 1<<20 || m.NetworkRX != 3000 || m.BlockWrite != 6 || m.RestartCount != 2 || m.Health != "healthy" {
		t.Fatal(m)
	}
}

func TestDockerMetricsFailureAndUnits(t *testing.T) {
	if _, err := containerMetrics(context.Background(), func(context.Context, ...string) (string, error) { return "", errors.New("daemon unavailable") }, "id"); err == nil {
		t.Fatal("daemon failure hidden")
	}
	for _, value := range []string{"bad", "-1MiB", "NaNB", "+InfB", "18446744073709551616B"} {
		if _, err := metricBytes(value); err == nil {
			t.Fatal(value, "accepted")
		}
	}
}
