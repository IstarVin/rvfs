package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCacheFile(t *testing.T, cl *cache.CacheLayer, cacheBase, remoteID, rel string, size int64, pinned bool) {
	t.Helper()
	seedCacheFileWithState(t, cl, cacheBase, remoteID, rel, size, pinned, cache.StateClean)
}

func seedCacheFileWithState(t *testing.T, cl *cache.CacheLayer, cacheBase, remoteID, rel string, size int64, pinned bool, state cache.FileState) {
	t.Helper()

	dp := filepath.Join(cacheBase, remoteID, "files", rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dp), 0755))
	f, err := os.Create(dp)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path:         rel,
		Size:         size,
		Mode:         0100644,
		State:        state,
		CachedRanges: "0-1",
		Pinned:       pinned,
		LastAccess:   time.Now().Unix(),
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

func TestCacheClean_IncludesDownloadingAndEvicted(t *testing.T) {
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

	seedCacheFileWithState(t, cl, cacheDir, "demo", "clean.txt", 16, false, cache.StateClean)
	seedCacheFileWithState(t, cl, cacheDir, "demo", "downloading.txt", 24, false, cache.StateDownloading)
	seedCacheFileWithState(t, cl, cacheDir, "demo", "evicted.txt", 32, false, cache.StateEvicted)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	err = cacheCleanCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	clean, err := cl.DB().GetFile("clean.txt")
	require.NoError(t, err)
	require.NotNil(t, clean)
	assert.Equal(t, cache.StateEvicted, clean.State)

	downloading, err := cl.DB().GetFile("downloading.txt")
	require.NoError(t, err)
	require.NotNil(t, downloading)
	assert.Equal(t, cache.StateEvicted, downloading.State)

	evicted, err := cl.DB().GetFile("evicted.txt")
	require.NoError(t, err)
	require.NotNil(t, evicted)
	assert.Equal(t, cache.StateEvicted, evicted.State)

	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "clean.txt"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "downloading.txt"))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cacheDir, "demo", "files", "evicted.txt"))
	assert.True(t, os.IsNotExist(err))

	assert.Contains(t, out.String(), "Candidates:")
	assert.Contains(t, out.String(), "3")
}

func TestCacheClean_ResetsRangesAndTransientMetadata(t *testing.T) {
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

	dp := filepath.Join(cacheDir, "demo", "files", "partial.bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(dp), 0755))
	require.NoError(t, os.WriteFile(dp, []byte("0123456789"), 0644))

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path:         "partial.bin",
		Size:         10,
		Mode:         0100644,
		State:        cache.StateDownloading,
		CachedRanges: "0-4",
		SyncError:    "transient",
		RetryAfter:   123,
		Pinned:       false,
		LastAccess:   time.Now().Unix(),
	}))

	cmd := &cobra.Command{}
	err = cacheCleanCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	entry, err := cl.DB().GetFile("partial.bin")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, cache.StateEvicted, entry.State)
	assert.Equal(t, "", entry.CachedRanges)
	assert.Equal(t, "", entry.SyncError)
	assert.Equal(t, int64(0), entry.RetryAfter)
}

func TestCacheClean_ScopedToSourceDirectory(t *testing.T) {
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

	seedCacheFile(t, cl, cacheDir, "demo", "Videos/Drama/ep1.mkv", 16, false)
	seedCacheFile(t, cl, cacheDir, "demo", "Videos/Other/ep2.mkv", 32, false)

	cmd := &cobra.Command{}
	err = cacheCleanCmd.RunE(cmd, []string{"demo:Videos/Drama"})
	require.NoError(t, err)

	drama, err := cl.DB().GetFile("Videos/Drama/ep1.mkv")
	require.NoError(t, err)
	require.NotNil(t, drama)
	assert.Equal(t, cache.StateEvicted, drama.State)

	other, err := cl.DB().GetFile("Videos/Other/ep2.mkv")
	require.NoError(t, err)
	require.NotNil(t, other)
	assert.Equal(t, cache.StateClean, other.State)
}

func TestCacheClean_ScopedToMountPathDirectory(t *testing.T) {
	prevCfg := globalCfg
	prevInclude := cacheCleanIncludePinned
	t.Cleanup(func() {
		globalCfg = prevCfg
		cacheCleanIncludePinned = prevInclude
	})

	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	cacheDir := t.TempDir()
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: cacheDir}}
	cacheCleanIncludePinned = false

	cl, err := cache.NewCacheLayer(cacheDir, "gdrive")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	seedCacheFile(t, cl, cacheDir, "gdrive", "Videos/Drama/ep1.mkv", 16, false)
	seedCacheFile(t, cl, cacheDir, "gdrive", "Videos/Other/ep2.mkv", 32, false)

	mountpoint := filepath.Join(t.TempDir(), "mnt", "gdrive")
	require.NoError(t, os.MkdirAll(filepath.Join(mountpoint, "Videos", "Drama"), 0755))

	reg, err := ipc.OpenMountRegistry()
	require.NoError(t, err)
	t.Cleanup(func() { _ = reg.Close() })

	require.NoError(t, reg.Register(ipc.MountEntry{
		Mountpoint: mountpoint,
		Source:     "gdrive:",
		RemoteName: "gdrive",
		SockPath:   filepath.Join(runtimeDir, "rvfs", "gdrive.sock"),
		PID:        os.Getpid(),
		MountedAt:  time.Now().Unix(),
	}))

	cmd := &cobra.Command{}
	err = cacheCleanCmd.RunE(cmd, []string{filepath.Join(mountpoint, "Videos", "Drama")})
	require.NoError(t, err)

	drama, err := cl.DB().GetFile("Videos/Drama/ep1.mkv")
	require.NoError(t, err)
	require.NotNil(t, drama)
	assert.Equal(t, cache.StateEvicted, drama.State)

	other, err := cl.DB().GetFile("Videos/Other/ep2.mkv")
	require.NoError(t, err)
	require.NotNil(t, other)
	assert.Equal(t, cache.StateClean, other.State)
}
