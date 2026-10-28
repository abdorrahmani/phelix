package monitor

import (
	"context"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/server"
)

// AppTraffic is one application's live request traffic, read from the proxy
// daemon. Counters are cumulative since the daemon started.
type AppTraffic struct {
	AppID      string
	AppName    string
	ServerID   string
	PublicPort int
	InFlight   int64
	Requests   int64
	Errors     int64
	P95        time.Duration
	Backends   []BackendTraffic
}

// BackendTraffic is one routing target's share of an app's traffic.
type BackendTraffic struct {
	Label    string
	Host     string
	Requests int64
	Errors   int64
	P95      time.Duration
	Share    float64
}

// ErrorRate returns the app's failed-request share, in percent.
func (t AppTraffic) ErrorRate() float64 {
	if t.Requests <= 0 {
		return 0
	}
	return 100 * float64(t.Errors) / float64(t.Requests)
}

// ErrorRate returns this backend's failed-request share, in percent.
func (b BackendTraffic) ErrorRate() float64 {
	if b.Requests <= 0 {
		return 0
	}
	return 100 * float64(b.Errors) / float64(b.Requests)
}

// trafficClient is the slice of proxy.Client traffic collection needs.
type trafficClient interface {
	Ping(ctx context.Context) error
	Status(ctx context.Context, appName string) ([]proxy.AppStatus, error)
	Stats(ctx context.Context, appName string) ([]proxy.BackendStat, error)
}

// newTrafficClient dials the proxy control socket. A var so tests can inject
// a fake client.
var newTrafficClient = func() trafficClient {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return nil
	}
	return proxy.NewClient(socket)
}

// CollectAppTraffic returns the traffic of every watched app enrolled with
// the proxy daemon. Apps without a proxy entry produce nothing: absence means
// "not proxied", never "zero traffic".
func CollectAppTraffic() []AppTraffic {
	client := newTrafficClient()
	if client == nil {
		return nil
	}
	return collectAppTraffic(client)
}

func collectAppTraffic(client trafficClient) []AppTraffic {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx); err != nil {
		return nil
	}
	statuses, err := client.Status(ctx, "")
	if err != nil {
		logs.Error("monitor", "failed to read proxy status: %v", err)
		return nil
	}
	stats, err := client.Stats(ctx, "")
	if err != nil {
		logs.Error("monitor", "failed to read proxy stats: %v", err)
		return nil
	}

	enrolled := make(map[string]int, len(statuses))
	inFlight := make(map[string]int64, len(statuses))
	for _, s := range statuses {
		enrolled[s.AppName] = s.PublicPort
		inFlight[s.AppName] = s.InFlight
	}

	hosts := make(map[string]map[string]bool)
	for _, s := range statuses {
		set := make(map[string]bool, len(s.Backends)+1)
		set[s.Primary.Host] = true
		for _, b := range s.Backends {
			set[b.Host] = true
		}
		hosts[s.AppName] = set
	}

	serverID := server.GetServerID()
	out := make([]AppTraffic, 0, len(statuses))
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		port, ok := enrolled[a.Name]
		if !ok {
			continue
		}
		backends := make([]proxy.BackendStat, 0)
		for _, s := range stats {
			if hosts[a.Name][s.Host] {
				backends = append(backends, s)
			}
		}
		out = append(out, buildAppTraffic(a.ID, a.Name, serverID, port, inFlight[a.Name], backends))
	}
	return out
}

func buildAppTraffic(appID, appName, serverID string, port int, inFlight int64, backends []proxy.BackendStat) AppTraffic {
	summary := proxy.Summarize(backends)
	t := AppTraffic{
		AppID:      appID,
		AppName:    appName,
		ServerID:   serverID,
		PublicPort: port,
		InFlight:   inFlight,
		Requests:   summary.Requests,
		Errors:     summary.Errors,
		P95:        summary.P95,
		Backends:   make([]BackendTraffic, 0, len(backends)),
	}
	for _, b := range backends {
		t.Backends = append(t.Backends, BackendTraffic{
			Label:    b.Label,
			Host:     b.Host,
			Requests: b.Requests,
			Errors:   b.Errors,
			P95:      b.P95(),
			Share:    b.Share(summary.Requests),
		})
	}
	return t
}
