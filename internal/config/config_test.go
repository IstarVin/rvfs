package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Load / Save
// ---------------------------------------------------------------------------

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	require.NoError(t, err)
	assert.NotNil(t, cfg.Remotes)
	assert.Empty(t, cfg.Remotes)
}

func TestLoadInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte("[[not valid toml"), 0644))

	_, err := Load(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse config")
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	original := &Config{
		Mount: MountConfig{
			Debug:            true,
			CacheDir:         "/tmp/cache",
			PollInterval:     Duration(2 * time.Minute),
			ProbeInterval:    Duration(10 * time.Second),
			RecoveryInterval: Duration(3 * time.Second),
			ReadAhead:        ByteSize(128 * 1024 * 1024),
			IdleTimeout:      Duration(30 * time.Second),
			ConflictStrategy: "local-wins",
		},
		Log: LogConfig{
			Level:  "debug",
			Format: "json",
			File:   "/var/log/rvfs.log",
		},
		Remotes: map[string]RemoteConfig{
			"gdrive1": {
				Type:         "gdrive",
				ClientID:     "id-123",
				ClientSecret: "secret-456",
				RootPath:     "/my/root",
			},
			"gdrive2": {
				Type:     "gdrive",
				ClientID: "id-abc",
			},
		},
	}

	require.NoError(t, original.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, original.Mount, loaded.Mount)
	assert.Equal(t, original.Log, loaded.Log)
	assert.Equal(t, original.Remotes, loaded.Remotes)
}

func TestSaveCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "config.toml")

	cfg := &Config{Remotes: map[string]RemoteConfig{}}
	require.NoError(t, cfg.Save(path))

	_, err := os.Stat(path)
	assert.NoError(t, err)
}

func TestSavePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	cfg := &Config{Remotes: map[string]RemoteConfig{}}
	require.NoError(t, cfg.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestSaveAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	cfg := &Config{Remotes: map[string]RemoteConfig{
		"test": {Type: "gdrive"},
	}}
	require.NoError(t, cfg.Save(path))

	// Temp file should not linger after a successful save.
	_, err := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err), "temp file should be cleaned up")
}

func TestLoadEmptyRemotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[remotes]\n"), 0644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.NotNil(t, cfg.Remotes)
	assert.Empty(t, cfg.Remotes)
}

func TestSaveProducesValidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	cfg := &Config{Remotes: map[string]RemoteConfig{
		"r1": {Type: "gdrive", ClientID: "cid", ClientSecret: "cs", RootPath: "/path"},
	}}
	require.NoError(t, cfg.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var roundtrip Config
	err = toml.Unmarshal(data, &roundtrip)
	assert.NoError(t, err, "saved config should be valid TOML")
}

func TestSaveOverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	cfg1 := &Config{Remotes: map[string]RemoteConfig{"old": {Type: "gdrive"}}}
	require.NoError(t, cfg1.Save(path))

	cfg2 := &Config{Remotes: map[string]RemoteConfig{"new": {Type: "s3"}}}
	require.NoError(t, cfg2.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Contains(t, loaded.Remotes, "new")
	assert.NotContains(t, loaded.Remotes, "old")
}

// ---------------------------------------------------------------------------
// DefaultPath / TokenPath
// ---------------------------------------------------------------------------

func TestDefaultPathNotEmpty(t *testing.T) {
	p := DefaultPath()
	assert.NotEmpty(t, p)
	assert.Contains(t, p, "rvfs")
	assert.Contains(t, p, ".toml")
}

func TestTokenPathContainsRemoteName(t *testing.T) {
	p := TokenPath("myremote")
	assert.Contains(t, p, "myremote.json")
	assert.Contains(t, p, "tokens")
}

// ---------------------------------------------------------------------------
// Defaults populated from missing file
// ---------------------------------------------------------------------------

func TestDefaultsPopulatedOnMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	require.NoError(t, err)

	def := DefaultMountConfig()
	assert.Equal(t, def.PollInterval, cfg.Mount.PollInterval)
	assert.Equal(t, def.ProbeInterval, cfg.Mount.ProbeInterval)
	assert.Equal(t, def.RecoveryInterval, cfg.Mount.RecoveryInterval)
	assert.Equal(t, def.ReadAhead, cfg.Mount.ReadAhead)
	assert.Equal(t, def.ConflictStrategy, cfg.Mount.ConflictStrategy)

	defLog := DefaultLogConfig()
	assert.Equal(t, defLog.Level, cfg.Log.Level)
	assert.Equal(t, defLog.Format, cfg.Log.Format)
}

func TestPartialMountConfigPreservesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	// Only override poll_interval; everything else should remain at defaults.
	require.NoError(t, os.WriteFile(path, []byte(`
[mount]
poll_interval = "5m"
`), 0644))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, Duration(5*time.Minute), cfg.Mount.PollInterval)
	// Defaults untouched.
	assert.Equal(t, DefaultMountConfig().ProbeInterval, cfg.Mount.ProbeInterval)
	assert.Equal(t, DefaultMountConfig().ConflictStrategy, cfg.Mount.ConflictStrategy)
}

// ---------------------------------------------------------------------------
// Duration type
// ---------------------------------------------------------------------------

func TestDurationMarshalRoundTrip(t *testing.T) {
	cases := []time.Duration{
		0,
		5 * time.Nanosecond,
		30 * time.Second,
		5 * time.Minute,
		2 * time.Hour,
	}
	for _, d := range cases {
		original := Duration(d)
		text, err := original.MarshalText()
		require.NoError(t, err)

		var decoded Duration
		require.NoError(t, decoded.UnmarshalText(text))
		assert.Equal(t, original, decoded, "duration %v round-trip failed", d)
	}
}

func TestDurationUnmarshalCaseInsensitive(t *testing.T) {
	var d Duration
	require.NoError(t, d.UnmarshalText([]byte("30S")))
	assert.Equal(t, Duration(30*time.Second), d)
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	var d Duration
	err := d.UnmarshalText([]byte("notaduration"))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// ByteSize type
// ---------------------------------------------------------------------------

func TestByteSizeMarshalRoundTrip(t *testing.T) {
	cases := []struct {
		size    ByteSize
		wantStr string
	}{
		{0, "0"},
		{ByteSize(1024), "1K"},
		{ByteSize(4 * 1024 * 1024), "4M"},
		{ByteSize(2 * 1024 * 1024 * 1024), "2G"},
		{ByteSize(1500), "1500"}, // not a clean multiple
	}
	for _, tc := range cases {
		text, err := tc.size.MarshalText()
		require.NoError(t, err)
		assert.Equal(t, tc.wantStr, string(text))

		var decoded ByteSize
		require.NoError(t, decoded.UnmarshalText(text))
		assert.Equal(t, tc.size, decoded)
	}
}

func TestByteSizeUnmarshalSuffixes(t *testing.T) {
	cases := []struct {
		input string
		want  ByteSize
	}{
		{"64M", ByteSize(64 * 1024 * 1024)},
		{"64m", ByteSize(64 * 1024 * 1024)},
		{"1G", ByteSize(1 << 30)},
		{"256K", ByteSize(256 * 1024)},
		{"0", 0},
		{"1024", ByteSize(1024)},
	}
	for _, tc := range cases {
		var b ByteSize
		require.NoError(t, b.UnmarshalText([]byte(tc.input)), "input: %s", tc.input)
		assert.Equal(t, tc.want, b, "input: %s", tc.input)
	}
}

func TestByteSizeUnmarshalInvalid(t *testing.T) {
	var b ByteSize
	err := b.UnmarshalText([]byte("notasize"))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// LogConfig round-trip
// ---------------------------------------------------------------------------

func TestLogConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	original := &Config{
		Mount: DefaultMountConfig(),
		Log: LogConfig{
			Level:  "warn",
			Format: "json",
			File:   "/tmp/app.log",
		},
		Remotes: map[string]RemoteConfig{},
	}

	require.NoError(t, original.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, original.Log, loaded.Log)
}
