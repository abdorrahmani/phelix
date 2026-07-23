package grpc

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 30 * time.Second
	backoffFactor  = 2.0
	jitterPercent  = 0.25
)

// reconnectState manages exponential backoff reconnection.
type reconnectState struct {
	mu           sync.Mutex
	attempts     int
	currentDelay time.Duration
	resetTimer   *time.Timer
}

func newReconnectState() *reconnectState {
	return &reconnectState{
		currentDelay: initialBackoff,
	}
}

// nextDelay returns the next backoff delay with jitter and increments the attempt counter.
func (r *reconnectState) nextDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	delay := r.currentDelay

	// Apply jitter: ±jitterPercent of the delay
	jitter := float64(delay) * jitterPercent
	delta := rand.Float64()*2*jitter - jitter
	delay = time.Duration(float64(delay) + delta)

	// Ensure minimum of 100ms
	if delay < 100*time.Millisecond {
		delay = 100 * time.Millisecond
	}

	// Exponential increase for next attempt
	r.currentDelay = time.Duration(float64(r.currentDelay) * backoffFactor)
	if r.currentDelay > maxBackoff {
		r.currentDelay = maxBackoff
	}

	r.attempts++
	return delay
}

// reset clears the backoff state after a successful connection.
func (r *reconnectState) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = 0
	r.currentDelay = initialBackoff
}

// attempts returns the current attempt count.
func (r *reconnectState) getAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// startReconnectLoop runs the reconnection loop in the background.
// It calls connectFn when reconnection is needed, and logs state changes.
func (c *Client) startReconnectLoop() {
	for {
		select {
		case <-c.done:
			return
		case <-c.reconnectCh:
			c.attemptReconnect()
		}
	}
}

func (c *Client) attemptReconnect() {
	delay := c.reconnect.nextDelay()
	grpcLog("[gRPC] Reconnecting in %v (attempt %d)...", delay, c.reconnect.getAttempts())

	select {
	case <-c.done:
		return
	case <-time.After(delay):
	}

	if err := c.Connect(); err != nil {
		grpcLog("[gRPC] Reconnection attempt %d failed: %v", c.reconnect.getAttempts(), err)
		// Signal another reconnection attempt
		select {
		case c.reconnectCh <- struct{}{}:
		default:
		}
		return
	}

	c.reconnect.reset()
	grpcLog("[gRPC] Successfully reconnected")
}

// scheduleReconnect signals that a reconnection is needed.
func (c *Client) scheduleReconnect() {
	select {
	case c.reconnectCh <- struct{}{}:
	default:
	}
}

// backoffDuration returns a capped exponential backoff for external callers.
func backoffDuration(attempt int) time.Duration {
	d := float64(initialBackoff) * math.Pow(backoffFactor, float64(attempt))
	if d > float64(maxBackoff) {
		d = float64(maxBackoff)
	}
	jitter := d * jitterPercent
	delta := rand.Float64()*2*jitter - jitter
	d += delta
	if d < 100 {
		d = 100
	}
	return time.Duration(d)
}
