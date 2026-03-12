package sync

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/remote"
	"github.com/IstarVin/rvfs/internal/testutil"
)

func newTestCacheLayer(t *testing.T) *cache.CacheLayer {
	t.Helper()
	cl, err := cache.NewCacheLayer(t.TempDir(), "test-remote")
	require.NoError(t, err)
	t.Cleanup(func() { cl.Close() })
	return cl
}

// ---------- ValidConflictStrategy ----------

func TestValidConflictStrategy(t *testing.T) {
	for _, s := range []string{"both", "local-wins", "remote-wins", "manual"} {
		assert.True(t, ValidConflictStrategy(s), "should be valid: %s", s)
	}
	for _, s := range []string{"", "invalid", "BOTH", "none"} {
		assert.False(t, ValidConflictStrategy(s), "should be invalid: %s", s)
	}
}

// ---------- Resolve: local-wins ----------

func TestResolveLocalWins(t *testing.T) {
	cl := newTestCacheLayer(t)
	adapter := &testutil.MockRemoteAdapter{}
	resolver := NewResolver(StrategyLocalWins, cl, adapter)

	// Seed a dirty entry.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "f.txt", State: cache.StateSyncing, Mode: 0100644,
		LocalMtime: 100, RemoteMtime: 90, SyncError: "prev error",
	}))

	entry, err := cl.DB().GetFile("f.txt")
	require.NoError(t, err)

	remoteStat := remote.FileInfo{
		Path: "f.txt", Size: 50, Mtime: time.Unix(200, 0), Checksum: "abc",
	}
	require.NoError(t, resolver.Resolve(entry, remoteStat))

	got, err := cl.DB().GetFile("f.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateDirty, got.State)
	assert.Empty(t, got.SyncError, "SyncError should be cleared")
}

// ---------- Resolve: remote-wins ----------

func TestResolveRemoteWins(t *testing.T) {
	cl := newTestCacheLayer(t)
	remoteContent := []byte("remote content here")
	adapter := &testutil.MockRemoteAdapter{GetData: remoteContent}
	resolver := NewResolver(StrategyRemoteWins, cl, adapter)

	// Create the file on disk so DiskPath is valid.
	f, _, err := cl.Create("f.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("local content"))
	f.Close()

	entry, err := cl.DB().GetFile("f.txt")
	require.NoError(t, err)

	remoteStat := remote.FileInfo{
		Path: "f.txt", Size: int64(len(remoteContent)),
		Mtime: time.Unix(300, 0), Checksum: "sha-remote",
	}
	require.NoError(t, resolver.Resolve(entry, remoteStat))

	got, err := cl.DB().GetFile("f.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateClean, got.State)
	assert.Equal(t, int64(300), got.RemoteMtime)
	assert.Equal(t, int64(300), got.LocalMtime)
	assert.Equal(t, int64(len(remoteContent)), got.Size)
	assert.Equal(t, "sha-remote", got.Checksum)
	assert.Empty(t, got.SyncError)

	// Verify file contents on disk.
	data, err := os.ReadFile(cl.DiskPath("f.txt"))
	require.NoError(t, err)
	assert.Equal(t, remoteContent, data)
}

func TestResolveRemoteWinsDownloadFailure(t *testing.T) {
	cl := newTestCacheLayer(t)
	adapter := &testutil.MockRemoteAdapter{GetErr: errors.New("network error")}
	resolver := NewResolver(StrategyRemoteWins, cl, adapter)

	f, _, err := cl.Create("fail.txt", 0644)
	require.NoError(t, err)
	f.Close()

	entry, err := cl.DB().GetFile("fail.txt")
	require.NoError(t, err)

	remoteStat := remote.FileInfo{Path: "fail.txt", Mtime: time.Unix(500, 0)}
	err = resolver.Resolve(entry, remoteStat)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "network error")
}

// ---------- Resolve: both ----------

func TestResolveBoth(t *testing.T) {
	cl := newTestCacheLayer(t)
	remoteContent := []byte("remote version")
	adapter := &testutil.MockRemoteAdapter{GetData: remoteContent}
	resolver := NewResolver(StrategyBoth, cl, adapter)

	f, _, err := cl.Create("doc.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("local version"))
	f.Close()

	entry, err := cl.DB().GetFile("doc.txt")
	require.NoError(t, err)
	entry.LocalMtime = 100

	remoteStat := remote.FileInfo{
		Path: "doc.txt", Size: int64(len(remoteContent)),
		Mtime: time.Unix(200, 0), Checksum: "chk",
	}
	require.NoError(t, resolver.Resolve(entry, remoteStat))

	// Original should be marked conflict.
	got, err := cl.DB().GetFile("doc.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateConflict, got.State)

	// Sidecar should exist in DB as clean.
	sidecar, err := cl.DB().GetFile("doc.txt.conflict.200")
	require.NoError(t, err)
	require.NotNil(t, sidecar)
	assert.Equal(t, cache.StateClean, sidecar.State)
	assert.Equal(t, int64(len(remoteContent)), sidecar.Size)

	// Conflict should be recorded.
	conflicts, err := cl.DB().ListConflicts()
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "doc.txt", conflicts[0].Path)
}

func TestResolveBothDownloadFailure(t *testing.T) {
	cl := newTestCacheLayer(t)
	adapter := &testutil.MockRemoteAdapter{GetErr: errors.New("timeout")}
	resolver := NewResolver(StrategyBoth, cl, adapter)

	f, _, err := cl.Create("fail.txt", 0644)
	require.NoError(t, err)
	f.Close()

	entry, err := cl.DB().GetFile("fail.txt")
	require.NoError(t, err)

	remoteStat := remote.FileInfo{Path: "fail.txt", Mtime: time.Unix(400, 0)}
	// Should not return error — still marks conflict so user is notified.
	require.NoError(t, resolver.Resolve(entry, remoteStat))

	got, err := cl.DB().GetFile("fail.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateConflict, got.State)

	// Sidecar should NOT exist (download failed, cleaned up).
	sidecar, err := cl.DB().GetFile("fail.txt.conflict.400")
	require.NoError(t, err)
	assert.Nil(t, sidecar)
}

// ---------- Resolve: manual ----------

func TestResolveManual(t *testing.T) {
	cl := newTestCacheLayer(t)
	adapter := &testutil.MockRemoteAdapter{}
	resolver := NewResolver(StrategyManual, cl, adapter)

	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "m.txt", State: cache.StateDirty, Mode: 0100644,
		LocalMtime: 100, RemoteMtime: 50,
	}))

	entry, err := cl.DB().GetFile("m.txt")
	require.NoError(t, err)

	remoteStat := remote.FileInfo{Path: "m.txt", Mtime: time.Unix(200, 0)}
	require.NoError(t, resolver.Resolve(entry, remoteStat))

	got, err := cl.DB().GetFile("m.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateConflict, got.State)
	assert.Equal(t, int64(200), got.RemoteMtime)

	conflicts, err := cl.DB().ListConflicts()
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "m.txt", conflicts[0].Path)
}
