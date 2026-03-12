package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	require.NoError(t, err)
	assert.NotNil(t, cfg.Remotes)
	assert.Empty(t, cfg.Remotes)
}

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json!}"), 0644))

	_, err := Load(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse config")
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	original := &Config{
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
	assert.Equal(t, original.Remotes, loaded.Remotes)
}

func TestSaveCreatesParentDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "config.json")

	cfg := &Config{Remotes: map[string]RemoteConfig{}}
	require.NoError(t, cfg.Save(path))

	_, err := os.Stat(path)
	assert.NoError(t, err)
}

func TestSavePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := &Config{Remotes: map[string]RemoteConfig{}}
	require.NoError(t, cfg.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestSaveAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := &Config{Remotes: map[string]RemoteConfig{
		"test": {Type: "gdrive"},
	}}
	require.NoError(t, cfg.Save(path))

	// Temp file should not linger after a successful save.
	_, err := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err), "temp file should be cleaned up")
}

func TestLoadNilRemotesInitialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"remotes": null}`), 0644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.NotNil(t, cfg.Remotes, "nil remotes should be initialized to empty map")
}

func TestLoadEmptyRemotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"remotes": {}}`), 0644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.NotNil(t, cfg.Remotes)
	assert.Empty(t, cfg.Remotes)
}

func TestSaveProducesValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg := &Config{Remotes: map[string]RemoteConfig{
		"r1": {Type: "gdrive", ClientID: "cid", ClientSecret: "cs", RootPath: "/path"},
	}}
	require.NoError(t, cfg.Save(path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(data), "saved config should be valid JSON")
}

func TestSaveOverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	cfg1 := &Config{Remotes: map[string]RemoteConfig{"old": {Type: "gdrive"}}}
	require.NoError(t, cfg1.Save(path))

	cfg2 := &Config{Remotes: map[string]RemoteConfig{"new": {Type: "s3"}}}
	require.NoError(t, cfg2.Save(path))

	loaded, err := Load(path)
	require.NoError(t, err)
	assert.Contains(t, loaded.Remotes, "new")
	assert.NotContains(t, loaded.Remotes, "old")
}

func TestDefaultPathNotEmpty(t *testing.T) {
	p := DefaultPath()
	assert.NotEmpty(t, p)
	assert.Contains(t, p, "rvfs")
}

func TestTokenPathContainsRemoteName(t *testing.T) {
	p := TokenPath("myremote")
	assert.Contains(t, p, "myremote.json")
	assert.Contains(t, p, "tokens")
}
