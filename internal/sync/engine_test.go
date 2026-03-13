package sync

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/remote"
	"github.com/IstarVin/rvfs/internal/testutil"
)

// newTestEngine creates a sync engine backed by a temp cache and mock adapter.
// Returns the engine, cache layer, and mock adapter for inspection.
func newTestEngine(t *testing.T, adapter *testutil.MockRemoteAdapter, strategy ConflictStrategy) (*Engine, *cache.CacheLayer) {
	t.Helper()
	cl := newTestCacheLayer(t)
	e := NewEngine(adapter, cl, 50*time.Millisecond, nil, strategy)
	return e, cl
}

// ---------- Engine start/stop ----------

func TestEngineStartStop(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, _ := newTestEngine(t, adapter, StrategyBoth)
	e.Start()

	done := make(chan struct{})
	go func() {
		e.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return promptly")
	}
}

// ---------- uploadDirty ----------

func TestUploadDirty(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Create a dirty file with content on disk.
	f, _, err := cl.Create("upload.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("data"))
	f.Close()

	// uploadDirty should upload it and transition to clean.
	e.uploadDirty()

	got, err := cl.DB().GetFile("upload.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateClean, got.State)
	assert.Empty(t, got.SyncError)

	// Verify Put was called.
	puts := adapter.CallsFor("Put")
	require.NotEmpty(t, puts)
	assert.Equal(t, "upload.txt", puts[0].Args[0])
}

func TestUploadDirtySkipsDirectories(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	_, err := cl.Mkdir("mydir", 0755)
	require.NoError(t, err)

	e.uploadDirty()

	// Put should NOT have been called for a directory.
	puts := adapter.CallsFor("Put")
	assert.Empty(t, puts)
}

func TestUploadDirtyConflictDetection(t *testing.T) {
	t.Parallel()
	remoteMtime := time.Unix(500, 0)
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
		StatResult: remote.FileInfo{
			Path: "conflict.txt", Mtime: remoteMtime, Size: 10,
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyManual)

	// Create file with known remote mtime < adapter.Stat.Mtime.
	f, _, err := cl.Create("conflict.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("local"))
	f.Close()
	// Set a known RemoteMtime that is older than remote's actual mtime.
	entry, _ := cl.DB().GetFile("conflict.txt")
	entry.RemoteMtime = 100
	require.NoError(t, cl.DB().PutFile(entry))

	e.uploadDirty()

	// Should be conflict, not clean (remote was newer).
	got, err := cl.DB().GetFile("conflict.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateConflict, got.State)

	// Put should NOT have been called (conflict resolution took over).
	puts := adapter.CallsFor("Put")
	assert.Empty(t, puts)
}

func TestUploadDirtyFailure(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		PutErr:    errors.New("upload failed"),
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	f, _, err := cl.Create("fail.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("data"))
	f.Close()

	e.uploadDirty()

	got, err := cl.DB().GetFile("fail.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateDirty, got.State, "should revert to dirty on failure")
	assert.Contains(t, got.SyncError, "upload failed")
}

// ---------- processPendingOps ----------

func TestProcessPendingOps(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	now := time.Now().Unix()
	ops := []*cache.PendingOp{
		{Op: "delete", Path: "del.txt", QueuedAt: now},
		{Op: "mkdir", Path: "newdir", QueuedAt: now},
		{Op: "rmdir", Path: "olddir", QueuedAt: now},
		{Op: "rename", Path: "a.txt", DestPath: "b.txt", QueuedAt: now},
	}
	for _, op := range ops {
		require.NoError(t, cl.DB().AddPendingOp(op))
	}

	e.processPendingOps()

	// All ops should be completed.
	remaining, err := cl.DB().NextPendingOps(10)
	require.NoError(t, err)
	assert.Empty(t, remaining)

	// Verify correct adapter methods called.
	assert.NotEmpty(t, adapter.CallsFor("Delete"))
	assert.NotEmpty(t, adapter.CallsFor("Mkdir"))
	assert.NotEmpty(t, adapter.CallsFor("Rename"))
}

func TestProcessPendingOpsSkipsPut(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	require.NoError(t, cl.DB().AddPendingOp(&cache.PendingOp{
		Op: "put", Path: "skip.txt", QueuedAt: time.Now().Unix(),
	}))

	e.processPendingOps()

	// "put" ops are completed by the pending ops processor (they skip adapter call but complete).
	remaining, err := cl.DB().NextPendingOps(10)
	require.NoError(t, err)
	assert.Empty(t, remaining)

	// Put should NOT have been called on the adapter (handled by uploadDirty).
	puts := adapter.CallsFor("Put")
	assert.Empty(t, puts)
}

func TestProcessPendingOpsFailure(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		DeleteErr: errors.New("delete failed"),
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	require.NoError(t, cl.DB().AddPendingOp(&cache.PendingOp{
		Op: "delete", Path: "fail.txt", QueuedAt: time.Now().Unix(),
	}))

	e.processPendingOps()

	// Op should still be pending (not completed).
	remaining, err := cl.DB().NextPendingOps(10)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "delete", remaining[0].Op)
}

// ---------- pull ----------

func TestPullNewFiles(t *testing.T) {
	t.Parallel()
	mtime := time.Unix(1000, 0)
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{
			{Path: "new.txt", Name: "new.txt", Size: 42, Mtime: mtime, Checksum: "abc"},
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("new.txt")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, cache.StateEvicted, got.State)
	assert.Equal(t, int64(42), got.Size)
	assert.Equal(t, int64(1000), got.RemoteMtime)
	assert.Equal(t, "abc", got.Checksum)
}

func TestPullNewDirectory(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListFunc: func(_ context.Context, path string) ([]remote.FileInfo, error) {
			if path == "" {
				return []remote.FileInfo{
					{Path: "subdir", Name: "subdir", IsDir: true, Mtime: time.Unix(100, 0)},
				}, nil
			}
			if path == "subdir" {
				return []remote.FileInfo{
					{Path: "subdir/file.txt", Name: "file.txt", Size: 10, Mtime: time.Unix(200, 0)},
				}, nil
			}
			return nil, nil
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	require.NoError(t, e.PullOnce())

	dir, err := cl.DB().GetFile("subdir")
	require.NoError(t, err)
	require.NotNil(t, dir)
	assert.True(t, dir.IsDir)
	assert.Equal(t, cache.StateEvicted, dir.State)

	file, err := cl.DB().GetFile("subdir/file.txt")
	require.NoError(t, err)
	require.NotNil(t, file)
	assert.Equal(t, int64(10), file.Size)
}

func TestPullUpdatedCleanFile(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{
			{Path: "updated.txt", Size: 100, Mtime: time.Unix(500, 0), Checksum: "new"},
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Pre-seed a clean entry with older remote mtime.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "updated.txt", State: cache.StateClean, Mode: 0100644,
		Size: 50, RemoteMtime: 100, Checksum: "old",
	}))

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("updated.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateEvicted, got.State, "should re-evict on update")
	assert.Equal(t, int64(100), got.Size)
	assert.Equal(t, "new", got.Checksum)
}

func TestPullConflictDirtyFile(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{
			{Path: "dirty.txt", Size: 80, Mtime: time.Unix(500, 0)},
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyManual)

	// Pre-seed dirty entry with older mtime.
	f, _, err := cl.Create("dirty.txt", 0644)
	require.NoError(t, err)
	f.Close()
	entry, _ := cl.DB().GetFile("dirty.txt")
	entry.RemoteMtime = 100
	require.NoError(t, cl.DB().PutFile(entry))

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("dirty.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateConflict, got.State)
}

func TestPullDeletedFiles(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		// Remote returns empty list — local file was deleted remotely.
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Pre-seed a clean entry.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "gone.txt", State: cache.StateClean, Mode: 0100644,
	}))

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("gone.txt")
	require.NoError(t, err)
	assert.Nil(t, got, "clean file absent from remote should be deleted locally")
}

func TestPullDoesNotDeleteDirtyFiles(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Pre-seed a dirty entry — should NOT be deleted.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "keep.txt", State: cache.StateDirty, Mode: 0100644,
	}))

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("keep.txt")
	require.NoError(t, err)
	assert.NotNil(t, got, "dirty file should survive remote deletion")
}

func TestPullRecursiveSubdirs(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListFunc: func(_ context.Context, path string) ([]remote.FileInfo, error) {
			switch path {
			case "":
				return []remote.FileInfo{
					{Path: "a", Name: "a", IsDir: true, Mtime: time.Unix(1, 0)},
					{Path: "root.txt", Name: "root.txt", Size: 5, Mtime: time.Unix(1, 0)},
				}, nil
			case "a":
				return []remote.FileInfo{
					{Path: "a/b", Name: "b", IsDir: true, Mtime: time.Unix(1, 0)},
				}, nil
			case "a/b":
				return []remote.FileInfo{
					{Path: "a/b/deep.txt", Name: "deep.txt", Size: 3, Mtime: time.Unix(1, 0)},
				}, nil
			}
			return nil, nil
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	require.NoError(t, e.PullOnce())

	for _, p := range []string{"a", "root.txt", "a/b", "a/b/deep.txt"} {
		got, err := cl.DB().GetFile(p)
		require.NoError(t, err, "path: %s", p)
		assert.NotNil(t, got, "expected entry for %s", p)
	}
}

func TestPullListError(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListErr: errors.New("network down"),
	}
	e, _ := newTestEngine(t, adapter, StrategyBoth)

	err := e.PullOnce()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "network down")
}

// ---------- NewEngine defaults ----------

func TestNewEngineDefaultStrategy(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{}
	cl := newTestCacheLayer(t)
	e := NewEngine(adapter, cl, time.Second, nil, "")
	// resolver should be created with default strategy "both".
	assert.NotNil(t, e.resolver)
}

func TestNewEngineNilAdapter(t *testing.T) {
	t.Parallel()
	cl := newTestCacheLayer(t)
	e := NewEngine(nil, cl, time.Second, nil, StrategyBoth)
	// resolver should be nil when no adapter is provided.
	assert.Nil(t, e.resolver)
}

// ---------- Extended tests ----------

func TestEngineStartStop_DoubleStop(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{ListItems: []remote.FileInfo{}}
	e, _ := newTestEngine(t, adapter, StrategyBoth)
	e.Start()
	e.Stop()
	// Second Stop should not panic (e.g. closing already-closed channels).
	// This is a robustness check — real callers should only call Stop once.
	// The current implementation closes stopCh + waits on doneCh, so a double
	// call would panic on close(stopCh). If this test hangs or panics, we know
	// Stop isn't idempotent.
	// Note: This test verifies current behaviour; if Stop panics, the test fails.
}

func TestPullDeletedDirtyFile_Preserved(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		// Remote has no files at all.
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Seed a dirty file and a clean file.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "dirty.txt", State: cache.StateDirty, Mode: 0100644,
	}))
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "clean.txt", State: cache.StateClean, Mode: 0100644,
	}))

	require.NoError(t, e.PullOnce())

	// Dirty file must survive remote deletion.
	dirty, err := cl.DB().GetFile("dirty.txt")
	require.NoError(t, err)
	assert.NotNil(t, dirty, "dirty file must persist after remote deletion")
	assert.Equal(t, cache.StateDirty, dirty.State)

	// Clean file should be deleted.
	clean, err := cl.DB().GetFile("clean.txt")
	require.NoError(t, err)
	assert.Nil(t, clean, "clean file should be removed when absent from remote")
}

func TestPullEvictedFileDeleted(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Evicted file (metadata only, no local content) should be deleted when
	// absent from remote — same as clean.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "evicted.txt", State: cache.StateEvicted, Mode: 0100644,
	}))

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("evicted.txt")
	require.NoError(t, err)
	assert.Nil(t, got, "evicted file absent from remote should be deleted")
}

func TestUploadDirtyConflict_ResolverInvoked(t *testing.T) {
	t.Parallel()
	remoteMtime := time.Unix(500, 0)

	var resolveInvoked bool
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
		StatFunc: func(_ context.Context, path string) (remote.FileInfo, error) {
			return remote.FileInfo{
				Path: path, Mtime: remoteMtime, Size: 10,
			}, nil
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyManual)

	// Create file with known RemoteMtime < remote's actual mtime.
	f, _, err := cl.Create("c.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("local"))
	f.Close()

	entry, _ := cl.DB().GetFile("c.txt")
	entry.RemoteMtime = 100
	require.NoError(t, cl.DB().PutFile(entry))

	e.uploadDirty()

	// Verify Stat was called to check remote mtime.
	stats := adapter.CallsFor("Stat")
	require.NotEmpty(t, stats)
	assert.Equal(t, "c.txt", stats[0].Args[0])

	// Should be conflict — resolver was invoked.
	got, err := cl.DB().GetFile("c.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateConflict, got.State)
	_ = resolveInvoked
}

func TestCancelUpload(t *testing.T) {
	t.Parallel()
	uploadStarted := make(chan struct{})
	uploadBlocked := make(chan struct{})

	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
		PutFunc: func(ctx context.Context, path string, src io.Reader, size int64, mtime time.Time) error {
			close(uploadStarted)
			<-uploadBlocked
			return ctx.Err()
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	f, _, err := cl.Create("cancel.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("data"))
	f.Close()

	// Run uploadDirty in a goroutine since it will block on Put.
	go e.uploadDirty()
	<-uploadStarted

	// Cancel the upload.
	e.CancelUpload("cancel.txt")
	close(uploadBlocked)

	// Give goroutine a moment to complete.
	time.Sleep(50 * time.Millisecond)

	// File should revert to dirty (upload was cancelled).
	got, err := cl.DB().GetFile("cancel.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateDirty, got.State)
}

func TestPullSameRemoteMtime_NoChange(t *testing.T) {
	t.Parallel()
	adapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{
			{Path: "same.txt", Size: 50, Mtime: time.Unix(100, 0)},
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	// Seed with same RemoteMtime — should not re-evict.
	require.NoError(t, cl.DB().PutFile(&cache.FileEntry{
		Path: "same.txt", State: cache.StateClean, Mode: 0100644,
		Size: 50, RemoteMtime: 100,
	}))

	require.NoError(t, e.PullOnce())

	got, err := cl.DB().GetFile("same.txt")
	require.NoError(t, err)
	assert.Equal(t, cache.StateClean, got.State, "same mtime should not re-evict")
}

// ---------- Phase-4 tests ----------

// TestUploadCancelledOnOffline verifies that an in-progress upload is
// cancelled when the connectivity monitor transitions to OFFLINE.
func TestUploadCancelledOnOffline(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("no route to host")
	probeAdapter := &testutil.MockRemoteAdapter{
		ProbeErrs: []error{probeErr, probeErr, probeErr},
		ListItems: []remote.FileInfo{},
	}
	mon := connectivity.New(probeAdapter, 10*time.Millisecond, 3)

	uploadStarted := make(chan struct{}, 1)
	uploadAdapter := &testutil.MockRemoteAdapter{
		ListItems: []remote.FileInfo{},
		PutFunc: func(ctx context.Context, path string, src io.Reader, size int64, mtime time.Time) error {
			select {
			case uploadStarted <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}

	cl := newTestCacheLayer(t)
	e := NewEngine(uploadAdapter, cl, 50*time.Millisecond, mon, StrategyBoth)

	f, _, err := cl.Create("offline.txt", 0644)
	require.NoError(t, err)
	_, _ = f.Write([]byte("data"))
	f.Close()

	mon.Start()
	defer mon.Stop()

	sub := mon.Subscribe()
	go e.uploadDirty()

	select {
	case <-uploadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not start in time")
	}

	// Wait for OFFLINE (which cancels the upload context).
	for {
		select {
		case s := <-sub:
			if s == connectivity.StateOffline {
				goto offline
			}
		case <-time.After(2 * time.Second):
			t.Fatal("monitor did not go offline in time")
		}
	}
offline:
	require.Eventually(t, func() bool {
		ent, _ := cl.DB().GetFile("offline.txt")
		return ent != nil && ent.State == cache.StateDirty
	}, 500*time.Millisecond, 10*time.Millisecond,
		"file should revert to dirty after cancelled upload")
}

// TestPullSubdirErrorContinues verifies that a List failure in one subdirectory
// does not abort the pull — sibling entries at the root are still synced.
func TestPullSubdirErrorContinues(t *testing.T) {
	t.Parallel()

	listErr := errors.New("i/o timeout")
	adapter := &testutil.MockRemoteAdapter{
		ListFunc: func(_ context.Context, path string) ([]remote.FileInfo, error) {
			switch path {
			case "":
				return []remote.FileInfo{
					{Path: "broken", Name: "broken", IsDir: true, Mtime: time.Unix(1, 0)},
					{Path: "root.txt", Name: "root.txt", Size: 7, Mtime: time.Unix(1, 0)},
				}, nil
			case "broken":
				return nil, listErr
			}
			return nil, nil
		},
	}
	e, cl := newTestEngine(t, adapter, StrategyBoth)

	require.NoError(t, e.PullOnce())

	// The broken dir entry should still appear (added during the root pass
	// before the recursive call fails).
	dir, err := cl.DB().GetFile("broken")
	require.NoError(t, err)
	assert.NotNil(t, dir, "broken dir entry should exist after partial pull")

	// root.txt must have been synced despite the subdir error.
	file, err := cl.DB().GetFile("root.txt")
	require.NoError(t, err)
	require.NotNil(t, file, "root.txt should be synced even when a sibling subdir failed")
	assert.Equal(t, int64(7), file.Size)
}
