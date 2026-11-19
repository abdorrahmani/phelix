package monitor

import (
	"context"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

type RuntimeMetricsCache struct {
	mu        sync.RWMutex
	samples   map[string]deploy.RuntimeMetrics
	collected time.Time
}

func NewRuntimeMetricsCache() *RuntimeMetricsCache {
	return &RuntimeMetricsCache{samples: map[string]deploy.RuntimeMetrics{}}
}

func (c *RuntimeMetricsCache) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		var ids []string
		for _, item := range app.Manager.ListApplications() {
			state, err := deploy.Load(item.Name)
			if err == nil {
				for _, instance := range dockerServingInstances(state) {
					ids = append(ids, instance.ContainerID)
				}
			}
		}
		if len(ids) > 0 {
			sampleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			samples, err := deploy.ContainerMetrics(sampleCtx, ids...)
			cancel()
			if err == nil {
				values := map[string]deploy.RuntimeMetrics{}
				for _, sample := range samples {
					values[sample.ContainerID] = sample
				}
				c.mu.Lock()
				c.samples = values
				c.collected = time.Now()
				c.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *RuntimeMetricsCache) Get(id string) (deploy.RuntimeMetrics, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sample, ok := c.samples[id]
	return sample, ok && time.Since(c.collected) < 15*time.Second
}
