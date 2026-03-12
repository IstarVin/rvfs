package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheDir(t *testing.T) {
	got := CacheDir("/base", "remote1")
	assert.Equal(t, filepath.Join("/base", "remote1"), got)
}

func TestCacheDirDifferentRemotes(t *testing.T) {
	a := CacheDir("/base", "remote-a")
	b := CacheDir("/base", "remote-b")
	assert.NotEqual(t, a, b)
}

func TestFilePath(t *testing.T) {
	got := FilePath("/base", "remote1", "docs/readme.md")
	assert.Equal(t, filepath.Join("/base", "remote1", "files", "docs/readme.md"), got)
}

func TestFilePathRoot(t *testing.T) {
	got := FilePath("/base", "remote1", "")
	assert.Equal(t, filepath.Join("/base", "remote1", "files"), got)
}

func TestEnsureLayout(t *testing.T) {
	base := t.TempDir()
	dbPath, err := EnsureLayout(base, "test-remote")
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(base, "test-remote", "meta.db"), dbPath)

	filesDir := filepath.Join(base, "test-remote", "files")
	info, err := os.Stat(filesDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestEnsureLayoutIdempotent(t *testing.T) {
	base := t.TempDir()

	dbPath1, err := EnsureLayout(base, "test-remote")
	require.NoError(t, err)

	dbPath2, err := EnsureLayout(base, "test-remote")
	require.NoError(t, err)

	assert.Equal(t, dbPath1, dbPath2)
}

func TestEnsureLayoutCreatesNestedDirs(t *testing.T) {
	base := filepath.Join(t.TempDir(), "deep", "path")
	_, err := EnsureLayout(base, "remote")
	require.NoError(t, err)

	filesDir := filepath.Join(base, "remote", "files")
	_, err = os.Stat(filesDir)
	assert.NoError(t, err)
}
