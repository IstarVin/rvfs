package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RemoteConfig stores the configuration for a single remote.
type RemoteConfig struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RootPath     string `json:"root_path"`
}

// Config holds all configured remotes.
type Config struct {
	Remotes map[string]RemoteConfig `json:"remotes"`
}

// DefaultPath returns the default config file location.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "rvfs", "config.json")
}

// TokenPath returns the token file path for a named remote.
func TokenPath(remoteName string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "rvfs", "tokens", remoteName+".json")
}

// Load reads the config from path.
// Returns an empty config (not an error) if the file does not exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{Remotes: make(map[string]RemoteConfig)}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Remotes == nil {
		cfg.Remotes = make(map[string]RemoteConfig)
	}
	return &cfg, nil
}

// Save writes the config to path atomically (write to temp file, then rename).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write config tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}
