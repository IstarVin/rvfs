package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCacheFile(t *testing.T, cl *cache.CacheLayer, cacheBase, remoteID, rel string, size int64, pinned bool) {
	t.Helper()

	dp := filepath.Join(cacheBase, remoteID, "files", rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dp), 0755))
	f, err := os.Create(dp)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path:       rel,
		Size:       size,
		Mode:       0100644,
		State:      cache.StateClean,
		Pinned:     pinned,
		LastAccess: time.Now().Unix(),
	}))
}

func TestCacheClean_DefaultSkipsPinned(t *testing.T) {
	prevCfg := globalCfg
	prevInclude := cacheCleanIncludePinned
	t.Cleanup(func() {
		globalCfg = prevCfg
		cacheCleanIncludePinned = prevInclude
	})

	cacheDir := t.TempDir()
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	cacheCleanIncludePinned = false

	cl, err := cache.NewCacheLayer(cacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	seedCacheFile(t, cl, cacheDir, "demo", "a.txt", 16, false)
	seedCacheFile(t, cl, cacheDir, "demo", "b.txt", 32, true)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)

	err = cacheCleanCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	a, err := cl.DB().GetFile("a.txt")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, cache.StateEvicted, a.State)

	b, err := cl.DB().GetFile("b.txt")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Equal(t, cache.StateClean, b.State)

	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "a.txt"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "b.txt"))
	assert.NoError(t, err)

	assert.Contains(t, out.String(), "Cache cleaned")
	assert.Contains(t, out.String(), "Candidates:")
	assert.Contains(t, out.String(), "false")
}

func TestCacheClean_IncludePinned(t *testing.T) {
	prevCfg := globalCfg
	prevInclude := cacheCleanIncludePinned
	t.Cleanup(func() {
		globalCfg = prevCfg
		cacheCleanIncludePinned = prevInclude
	})

	cacheDir := t.TempDir()
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	cacheCleanIncludePinned = true

	cl, err := cache.NewCacheLayer(cacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	seedCacheFile(t, cl, cacheDir, "demo", "a.txt", 16, false)
	seedCacheFile(t, cl, cacheDir, "demo", "b.txt", 32, true)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	err = cacheCleanCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	a, err := cl.DB().GetFile("a.txt")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, cache.StateEvicted, a.State)

	b, err := cl.DB().GetFile("b.txt")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Equal(t, cache.StateEvicted, b.State)

	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "a.txt"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "b.txt"))
	assert.True(t, os.IsNotExist(err))

	assert.Contains(t, out.String(), "Cache cleaned")
	assert.Contains(t, out.String(), "Candidates:")
	assert.Contains(t, out.String(), "true")
}
