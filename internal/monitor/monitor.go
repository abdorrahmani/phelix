package monitor

import (
	"sync"
)

// monitorService Main monitor service
type monitorService struct {
	connector         WebSocketConnector
	metricsCollector  MetricsCollector
	commandExecutor   CommandExecutor
	session           Session
	stopChan          chan struct{}
	mu                sync.Mutex
	reconnectAttempts int
	metricsPaused     bool
	metricsPauseMu    sync.Mutex
}

func NewMonitorService() MonitorService {
	return &monitorService{
		connector:        &websocketConn{},
		metricsCollector: &appMetricsCollector{},
		commandExecutor:  &appCommandExecutor{},
		stopChan:         make(chan struct{}),
	}
}

func (m *monitorService) SendCommand(cmd Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connector.WriteJSON(cmd)
}

func (m *monitorService) pauseMetrics() {
	m.metricsPauseMu.Lock()
	m.metricsPaused = true
	m.metricsPauseMu.Unlock()
}

func (m *monitorService) resumeMetrics() {
	m.metricsPauseMu.Lock()
	m.metricsPaused = false
	m.metricsPauseMu.Unlock()
}
