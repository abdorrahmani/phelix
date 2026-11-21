package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

func init() {
	app.ObserveDeployment = observeAppDeployment
}

func observeAppDeployment(info *app.AppInfo) error {
	state, err := deploy.Load(info.Name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == nil || (state.Mode != deploy.ModeBlueGreen && state.Mode != deploy.ModeRolling && state.Mode != deploy.ModeWorker) {
		return nil
	}
	d := deploy.DecideLifecycle(state)
	info.Status = d.Status
	info.PID = d.PID
	info.StartTicks = d.StartTicks
	info.Start = d.StartedAt
	info.Port = d.PublicPort
	if d.Alive == 0 {
		return nil
	}
	instance := state.ServingInstance()
	if instance != nil && instance.ContainerID != "" {
		info.RuntimeMetrics = func() (uint64, float64, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			samples, err := deploy.ContainerMetrics(ctx, instance.ContainerID)
			if err != nil {
				return 0, 0, err
			}
			if len(samples) != 1 || samples[0].State != "running" {
				return 0, 0, fmt.Errorf("container metrics unavailable for %s", info.Name)
			}
			return samples[0].MemoryBytes, samples[0].CPUPercent, nil
		}
	}
	return nil
}
