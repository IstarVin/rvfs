package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// ---------------------------------------------------------------------------
// Duration — a time.Duration that encodes/decodes as a human-readable string
// in TOML (e.g. "30s", "5m", "1h").
// ---------------------------------------------------------------------------

// Duration wraps time.Duration for TOML (de)serialisation.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(data []byte) error {
	v, err := time.ParseDuration(strings.ToLower(string(data)))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", data, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// ---------------------------------------------------------------------------
// ByteSize — an int64 that encodes/decodes with optional K/M/G suffixes.
// ---------------------------------------------------------------------------

// ByteSize wraps int64 for TOML (de)serialisation with K/M/G suffixes.
type ByteSize int64

func (b ByteSize) MarshalText() ([]byte, error) {
	v := int64(b)
	switch {
	case v == 0:
		return []byte("0"), nil
	case v%(1<<30) == 0:
		return []byte(strconv.FormatInt(v/(1<<30), 10) + "G"), nil
	case v%(1<<20) == 0:
		return []byte(strconv.FormatInt(v/(1<<20), 10) + "M"), nil
	case v%(1<<10) == 0:
		return []byte(strconv.FormatInt(v/(1<<10), 10) + "K"), nil
	default:
		return []byte(strconv.FormatInt(v, 10)), nil
	}
}

func (b *ByteSize) UnmarshalText(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "0" {
		*b = 0
		return nil
	}
	var multiplier int64 = 1
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, "G"):
		multiplier = 1 << 30
		s = s[:len(s)-1]
	case strings.HasSuffix(upper, "M"):
		multiplier = 1 << 20
		s = s[:len(s)-1]
	case strings.HasSuffix(upper, "K"):
		multiplier = 1 << 10
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: %w", data, err)
	}
	*b = ByteSize(n * multiplier)
	return nil
}

// Int64 returns the underlying byte count.
func (b ByteSize) Int64() int64 { return int64(b) }

// ---------------------------------------------------------------------------
// MountConfig — default values for the mount command.
// ---------------------------------------------------------------------------

// MountConfig holds default values for the mount command. All fields can be
// overridden by the corresponding CLI flags at runtime.
type MountConfig struct {
	// Debug enables verbose FUSE debug logging.
	Debug bool `toml:"debug"`
	// CacheDir overrides the default cache directory (~/.cache/rvfs).
	CacheDir string `toml:"cache_dir"`
	// PollInterval is how often the sync engine polls the remote for changes.
	PollInterval Duration `toml:"poll_interval"`
	// ProbeInterval is how often the connectivity monitor probes the remote.
	ProbeInterval Duration `toml:"probe_interval"`
	// RecoveryInterval is the faster probe interval used while offline.
	RecoveryInterval Duration `toml:"recovery_interval"`
	// ReadAhead is the number of bytes the sequential downloader stays ahead
	// of the read position. 0 means unlimited.
	ReadAhead ByteSize `toml:"read_ahead"`
	// IdleTimeout stops the sequential downloader after this duration with no
	// reads. 0 means wait indefinitely. Only effective when ReadAhead > 0.
	IdleTimeout Duration `toml:"idle_timeout"`
	// ConflictStrategy is one of: both, local-wins, remote-wins, manual.
	ConflictStrategy string `toml:"conflict_strategy"`
	// CacheSize is the maximum total size of the file cache. 0 means unlimited.
	CacheSize ByteSize `toml:"cache_size"`
	// CacheMaxAge evicts clean files that have not been accessed for longer
	// than this duration. 0 means never evict by age.
	CacheMaxAge Duration `toml:"cache_max_age"`
	// CacheMinFreeSpace is the minimum free space that must remain on the
	// filesystem containing the cache directory. Clean unpinned files are
	// evicted until this threshold is satisfied. 0 means disabled.
	CacheMinFreeSpace ByteSize `toml:"cache_min_free_space"`
}

// DefaultMountConfig returns a MountConfig pre-populated with the same
// defaults that the CLI uses when no config file is present.
func DefaultMountConfig() MountConfig {
	return MountConfig{
		PollInterval:     Duration(30 * time.Second),
		ProbeInterval:    Duration(5 * time.Second),
		RecoveryInterval: Duration(2 * time.Second),
		ReadAhead:        ByteSize(64 * 1024 * 1024), // 64 MiB
		IdleTimeout:      Duration(5),                // matches old mountIdleTimeout = 5
		ConflictStrategy: "both",
		CacheSize:        ByteSize(20 * 1024 * 1024 * 1024), // 20 GiB
	}
}

// ---------------------------------------------------------------------------
// LogConfig — logging configuration.
// ---------------------------------------------------------------------------

// LogConfig controls how the application writes logs.
type LogConfig struct {
	// Level is the minimum log level: debug, info, warn, error.
	Level string `toml:"level"`
	// Format is the output format: text or json.
	Format string `toml:"format"`
	// File is the path to write logs to. Empty means stderr.
	File string `toml:"file"`
}

// DefaultLogConfig returns a LogConfig with sensible defaults.
func DefaultLogConfig() LogConfig {
	return LogConfig{
		Level:  "info",
		Format: "text",
	}
}

// ---------------------------------------------------------------------------
// RemoteConfig / Config
// ---------------------------------------------------------------------------

// RemoteConfig stores the configuration for a single remote.
type RemoteConfig struct {
	Type         string `toml:"type"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	RootPath     string `toml:"root_path"`
}

// Config holds all application configuration.
type Config struct {
	Mount   MountConfig             `toml:"mount"`
	Log     LogConfig               `toml:"log"`
	Remotes map[string]RemoteConfig `toml:"remotes"`
}

// ---------------------------------------------------------------------------
// Paths
// ---------------------------------------------------------------------------

// DefaultPath returns the default config file location.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "rvfs", "config.toml")
}

// TokenPath returns the token file path for a named remote.
// Token files are kept in JSON format (OAuth2 library requirement).
func TokenPath(remoteName string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "rvfs", "tokens", remoteName+".json")
}

// ---------------------------------------------------------------------------
// Load / Save
// ---------------------------------------------------------------------------

// Load reads the config from path.
// Returns a config pre-populated with defaults (not an error) if the file
// does not exist.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Mount:   DefaultMountConfig(),
		Log:     DefaultLogConfig(),
		Remotes: make(map[string]RemoteConfig),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Remotes == nil {
		cfg.Remotes = make(map[string]RemoteConfig)
	}
	return cfg, nil
}

// Save writes the config to path atomically (write to temp file, then rename).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("write config tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}
