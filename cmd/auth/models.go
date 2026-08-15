package auth

import "time"

// Session holds the user's session information.
type Session struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SessionStatus represents the current authentication status.
type SessionStatus struct {
	User      string `json:"user"`
	SessionID string `json:"sessionID"`
	ExpiresAt string `json:"expiresAt"`
}

// AppDetail represents an application's runtime details.
type AppDetail struct {
	ID          uint      `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
	BuildStatus string    `json:"buildStatus"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
