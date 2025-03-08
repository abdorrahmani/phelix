package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/abdorrahmani/gophel/internal/config"
	"net/http"
)

type AuthRequest struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

type AuthResponse struct {
	Status       string `json:"status"`
	License      string `json:"license"`
	WebSocketURL string `json:"websocket_url"`
}

func Authenticate(username, token string) error {
	reqBody, _ := json.Marshal(AuthRequest{Username: username, Token: token})
	resp, err := http.Post("https://gophel.anophel.com/auth", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var authResp AuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return err
	}

	if authResp.Status != "success" {
		return errors.New("authentication failed")
	}

	config.SetAuthDetails(authResp.License, authResp.WebSocketURL)
	return nil
}
