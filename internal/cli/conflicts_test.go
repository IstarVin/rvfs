package cli

import (
	"bytes"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConflictsCommandUsesConfiguredCacheDir(t *testing.T) {
	prevCfg := globalCfg
	prevCacheDirFlag := conflictsCacheDir
	t.Cleanup(func() {
		globalCfg = prevCfg
		conflictsCacheDir = prevCacheDirFlag
	})

	configuredCacheDir := t.TempDir()
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: configuredCacheDir}}
	conflictsCacheDir = ""

	cl, err := cache.NewCacheLayer(configuredCacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })
	require.NoError(t, cl.DB().AddConflict("notes/todo.txt", 100, 200))

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	err = conflictsCmd.RunE(cmd, []string{"demo:"})
	require.NoError(t, err)

	assert.Contains(t, out.String(), "notes/todo.txt")
	assert.NotContains(t, out.String(), "No conflicts.")
}

func TestResolveCommandUsesConfiguredCacheDir(t *testing.T) {
	prevCfg := globalCfg
	prevCacheDirFlag := resolveCacheDir
	prevKeep := resolveKeep
	t.Cleanup(func() {
		globalCfg = prevCfg
		resolveCacheDir = prevCacheDirFlag
		resolveKeep = prevKeep
		resolveAll = false
	})

	configuredCacheDir := t.TempDir()
	globalCfg = &config.Config{Mount: config.MountConfig{CacheDir: configuredCacheDir}}
	resolveCacheDir = ""
	resolveKeep = "local"
	resolveAll = false

	cl, err := cache.NewCacheLayer(configuredCacheDir, "demo")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	path := "notes/todo.txt"
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path:  path,
		Mode:  0100644,
		State: cache.StateConflict,
	}))
	require.NoError(t, cl.DB().AddConflict(path, 100, 200))

	conflicts, err := cl.DB().ListConflicts()
	require.NoError(t, err)
	require.Len(t, conflicts, 1)

	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)

	err = resolveCmd.RunE(cmd, []string{"demo:", strconv.FormatInt(conflicts[0].ID, 10)})
	require.NoError(t, err)

	remaining, err := cl.DB().ListConflicts()
	require.NoError(t, err)
	assert.Len(t, remaining, 0)

	entry, err := cl.DB().GetFile(path)
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, cache.StateDirty, entry.State)
	assert.Contains(t, out.String(), filepath.Base(path))
}
