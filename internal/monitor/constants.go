package monitor

import "time"

const (
	pingInterval         = 15 * time.Second
	metricsInterval      = 2 * time.Second
	reconnectDelay       = 5 * time.Second
	maxReconnectAttempts = 3
	writeTimeout         = 10 * time.Second
	handshakeTimeout     = 45 * time.Second
	pongWait             = 60 * time.Second
)
