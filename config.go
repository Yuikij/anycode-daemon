package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// DefaultRelayURL is the AnyCode SaaS endpoint (HTTP API + WebSocket relay).
const DefaultRelayURL = "https://anycodeapp.com"

// Config is the daemon's persisted state at ~/.anycode/config.json.
type Config struct {
	RelayURL    string `json:"relayUrl"`              // e.g. https://anycodeapp.com
	Session     string `json:"session,omitempty"`     // account session token (anycode login)
	UserEmail   string `json:"userEmail,omitempty"`   // for display only
	DeviceID    string `json:"deviceId,omitempty"`    // server device id (if known)
	DeviceName  string `json:"deviceName,omitempty"`  //
	DeviceToken string `json:"deviceToken,omitempty"` // device connect-token (relay agent + JSON-RPC auth)
}

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".anycode")
}

func configPath() string {
	return filepath.Join(configDir(), "config.json")
}

func LoadConfig() *Config {
	cfg := &Config{RelayURL: DefaultRelayURL}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, cfg)
	if cfg.RelayURL == "" {
		cfg.RelayURL = DefaultRelayURL
	}
	return cfg
}

func (c *Config) Save() error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(), data, 0o600)
}
