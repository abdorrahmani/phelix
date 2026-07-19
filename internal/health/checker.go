package health

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Checker performs individual health checks
type Checker struct {
	httpClient *http.Client
}

// NewChecker creates a new health checker
func NewChecker() *Checker {
	return &Checker{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Check performs a health check on the given endpoint
func (c *Checker) Check(config *HealthCheckConfig) *HealthCheckResult {
	result := &HealthCheckResult{
		EndpointName: config.Name,
		URL:          config.URL,
		CheckedAt:    time.Now(),
	}

	// Parse timeout
	timeout := 10 * time.Second
	if config.Timeout != "" {
		if d, err := time.ParseDuration(config.Timeout); err == nil {
			timeout = d
		}
	}

	// Create a client with the specified timeout
	client := &http.Client{
		Timeout: timeout,
	}

	start := time.Now()
	statusCode, err := c.performCheck(config, client)
	latency := time.Since(start).Milliseconds()
	latencyPtr := latency
	result.LatencyMs = &latencyPtr

	if err != nil {
		errStr := err.Error()
		result.Error = &errStr
		result.Status = "DOWN"
		if strings.Contains(err.Error(), "context deadline exceeded") {
			result.Status = "TIMEOUT"
			result.LatencyMs = nil
		}
		return result
	}

	if statusCode != nil {
		result.StatusCode = statusCode
		if isStatusCodeHealthy(*statusCode, config.ExpectedCodes) {
			result.Status = "UP"
		} else {
			result.Status = "DOWN"
			errStr := fmt.Sprintf("unexpected status code: %d", *statusCode)
			result.Error = &errStr
		}
	} else {
		result.Status = "UP"
	}

	return result
}

// performCheck performs the actual check (HTTP or TCP)
func (c *Checker) performCheck(config *HealthCheckConfig, client *http.Client) (*int, error) {
	url := config.URL

	// Check if it's an HTTP(S) URL
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return c.httpCheck(url, client)
	}

	// Check if it's a host:port combination
	if strings.Contains(url, ":") {
		return c.tcpCheck(url, client.Timeout)
	}

	// Try to parse as TCP port on localhost
	if _, err := strconv.Atoi(url); err == nil {
		return c.tcpCheck("localhost:"+url, client.Timeout)
	}

	// Check if it's a PID
	if pid, err := strconv.Atoi(url); err == nil {
		return c.pidCheck(pid)
	}

	return nil, fmt.Errorf("invalid URL format: %s", url)
}

// httpCheck performs an HTTP health check
func (c *Checker) httpCheck(url string, client *http.Client) (*int, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer io.ReadAll(resp.Body)
	defer resp.Body.Close()

	return &resp.StatusCode, nil
}

// tcpCheck performs a TCP port check
func (c *Checker) tcpCheck(addr string, timeout time.Duration) (*int, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	conn.Close()

	statusCode := 200 // Assume 200 for successful TCP connection
	return &statusCode, nil
}

// pidCheck checks if a process with the given PID is running
func (c *Checker) pidCheck(pid int) (*int, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, fmt.Errorf("process not found: %v", err)
	}

	// Send signal 0 to check if process is alive (without actually sending a signal)
	err = process.Signal(os.Signal(nil))
	if err != nil {
		return nil, fmt.Errorf("process not responding: %v", err)
	}

	statusCode := 200
	return &statusCode, nil
}

// isStatusCodeHealthy checks if a status code falls within the expected range
func isStatusCodeHealthy(statusCode int, expectedCodes string) bool {
	if expectedCodes == "" {
		// Default to 200-299
		expectedCodes = "200-299"
	}

	// Parse ranges like "200-299" or "200,201,404"
	if strings.Contains(expectedCodes, "-") {
		parts := strings.Split(expectedCodes, "-")
		if len(parts) == 2 {
			start, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			end, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			return statusCode >= start && statusCode <= end
		}
	}

	// Check individual codes
	codes := strings.Split(expectedCodes, ",")
	for _, code := range codes {
		if expectedCode, _ := strconv.Atoi(strings.TrimSpace(code)); expectedCode == statusCode {
			return true
		}
	}

	return false
}
