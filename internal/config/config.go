package config

import "sync"

var (
	configLock     sync.Mutex
	CurrentLicense string
	WebSocketURL   string
)

func SetAuthDetails(license, wsURL string) {
	configLock.Lock()
	defer configLock.Unlock()
	CurrentLicense = license
	WebSocketURL = wsURL
}

func GetLicense() string {
	configLock.Lock()
	defer configLock.Unlock()
	return CurrentLicense
}

func GetWebSocketURL() string {
	configLock.Lock()
	defer configLock.Unlock()
	return WebSocketURL
}
