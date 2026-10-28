package monitor

import (
	"context"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

type fakeTrafficClient struct {
	statuses []proxy.AppStatus
	stats    []proxy.BackendStat
}

func (fakeTrafficClient) Ping(context.Context) error { return nil }

func (f fakeTrafficClient) Status(context.Context, string) ([]proxy.AppStatus, error) {
	return f.statuses, nil
}

func (f fakeTrafficClient) Stats(context.Context, string) ([]proxy.BackendStat, error) {
	return f.stats, nil
}

type trafficAppManager struct {
	app.AppManagerInterface
	items []app.AppListItem
}

func (m trafficAppManager) ListApplications() []app.AppListItem { return m.items }

func TestCollectAppTraffic(t *testing.T) {
	origMgr := app.Manager
	app.Manager = trafficAppManager{items: []app.AppListItem{
		{ID: "id-watched", Name: "watched", Watching: true},
		{ID: "id-unwatched", Name: "unwatched"},
		{ID: "id-not-proxied", Name: "not-proxied", Watching: true},
	}}
	t.Cleanup(func() { app.Manager = origMgr })

	client := fakeTrafficClient{
		statuses: []proxy.AppStatus{
			{AppName: "watched", PublicPort: 8080, InFlight: 3,
				Primary:  proxy.Target{Host: "127.0.0.1:9001", Label: "blue"},
				Backends: []proxy.Target{{Host: "127.0.0.1:9002", Label: "green"}}},
			{AppName: "unwatched", PublicPort: 8081},
		},
		stats: []proxy.BackendStat{
			{Host: "127.0.0.1:9001", Label: "blue", Requests: 30, Errors: 3},
			{Host: "127.0.0.1:9002", Label: "green", Requests: 10},
			{Host: "127.0.0.1:9003", Label: "other", Requests: 99},
		},
	}

	got := collectAppTraffic(client)
	if len(got) != 1 {
		t.Fatalf("traffic rows = %d, want 1 (only the watched, proxied app)", len(got))
	}
	traffic := got[0]
	if traffic.AppName != "watched" || traffic.PublicPort != 8080 || traffic.InFlight != 3 {
		t.Fatalf("unexpected app fields: %+v", traffic)
	}
	if traffic.Requests != 40 || traffic.Errors != 3 {
		t.Errorf("requests/errors = %d/%d, want 40/3", traffic.Requests, traffic.Errors)
	}
	if rate := traffic.ErrorRate(); rate != 7.5 {
		t.Errorf("error rate = %v, want 7.5", rate)
	}
	if len(traffic.Backends) != 2 {
		t.Fatalf("backends = %d, want 2 (other app's host excluded)", len(traffic.Backends))
	}
	if traffic.Backends[0].Share != 75 || traffic.Backends[1].Share != 25 {
		t.Errorf("shares = %v/%v, want 75/25", traffic.Backends[0].Share, traffic.Backends[1].Share)
	}
}
