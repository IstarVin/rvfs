package cache

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedCleanFile inserts a clean, unpinned file entry into the cache DB and
// creates a file of the given size on disk so eviction has something to remove.
func seedCleanFile(t *testing.T, cl *CacheLayer, path string, size int64, lastAccess int64) {
	t.Helper()

	dp := cl.diskPath(path)
	require.NoError(t, os.MkdirAll(filepath.Dir(dp), 0755))

	f, err := os.Create(dp)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	f.Close()

	require.NoError(t, cl.db.PutFile(&FileEntry{
		Path:       path,
		Size:       size,
		Mode:       0100644,
		State:      StateClean,
		CachePath:  dp,
		LastAccess: lastAccess,
	}))
}

// ---------- DirSize ----------

func TestDirSize_NestedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	os.MkdirAll(filepath.Join(dir, "sub"), 0755)
	os.WriteFile(filepath.Join(dir, "a.txt"), make([]byte, 100), 0644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), make([]byte, 200), 0644)

	got, err := DirSize(dir)
	require.NoError(t, err)
	assert.Equal(t, int64(300), got)
}

func TestDirSize_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	got, err := DirSize(dir)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got)
}

func TestDirSize_NonexistentDir(t *testing.T) {
	t.Parallel()
	_, err := DirSize("/nonexistent/dir/that/does/not/exist")
	assert.Error(t, err)
}

func TestDirUsage_NestedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0755))
	aPath := filepath.Join(dir, "a.txt")
	bPath := filepath.Join(dir, "sub", "b.txt")
	require.NoError(t, os.WriteFile(aPath, make([]byte, 100), 0644))
	require.NoError(t, os.WriteFile(bPath, make([]byte, 200), 0644))

	usage, err := DirUsage(dir)
	require.NoError(t, err)
	assert.Equal(t, int64(300), usage.LogicalBytes)
	assert.Equal(t, expectedPhysicalBytes(t, aPath)+expectedPhysicalBytes(t, bPath), usage.PhysicalBytes)
}

func TestDirUsage_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	usage, err := DirUsage(dir)
	require.NoError(t, err)
	assert.Equal(t, int64(0), usage.LogicalBytes)
	assert.Equal(t, int64(0), usage.PhysicalBytes)
}

func TestDirUsage_NonexistentDir(t *testing.T) {
	t.Parallel()
	_, err := DirUsage("/nonexistent/dir/that/does/not/exist")
	assert.Error(t, err)
}

func expectedPhysicalBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return st.Blocks * 512
}

// ---------- evictByAge ----------

func TestEvictByAge_OldFileEvicted(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	pastAccess := time.Now().Add(-2 * time.Hour).Unix()
	seedCleanFile(t, cl, "old.txt", 100, pastAccess)

	ev := &Evictor{MaxAge: 1 * time.Hour}
	ev.evictByAge(cl)

	entry, err := cl.db.GetFile("old.txt")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, StateEvicted, entry.State)

	// On-disk file should be removed.
	_, err = os.Stat(cl.diskPath("old.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestEvictByAge_RecentFileKept(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	recentAccess := time.Now().Unix()
	seedCleanFile(t, cl, "fresh.txt", 100, recentAccess)

	ev := &Evictor{MaxAge: 1 * time.Hour}
	ev.evictByAge(cl)

	entry, err := cl.db.GetFile("fresh.txt")
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, StateClean, entry.State)

	// File should still exist on disk.
	_, err = os.Stat(cl.diskPath("fresh.txt"))
	assert.NoError(t, err)
}

func TestEvictByAge_ZeroMaxAge_Noop(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	seedCleanFile(t, cl, "keep.txt", 100, 1) // very old access time

	ev := &Evictor{MaxAge: 0}
	ev.check(cl) // check should skip evictByAge since MaxAge == 0

	entry, err := cl.db.GetFile("keep.txt")
	require.NoError(t, err)
	assert.Equal(t, StateClean, entry.State)
}

// ---------- evictBySize ----------

func TestEvictBySize_EvictsLRU(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// Create two files; the one with the older access should be evicted first.
	seedCleanFile(t, cl, "oldest.txt", 500, 100)
	seedCleanFile(t, cl, "newer.txt", 500, 200)

	ev := &Evictor{MaxSize: 600} // total is 1000, so need to evict ~400+
	ev.evictBySize(cl)

	// Oldest should be evicted.
	oldest, err := cl.db.GetFile("oldest.txt")
	require.NoError(t, err)
	assert.Equal(t, StateEvicted, oldest.State)

	// Newer should remain (500 ≤ 600 after evicting oldest).
	newer, err := cl.db.GetFile("newer.txt")
	require.NoError(t, err)
	assert.Equal(t, StateClean, newer.State)
}

func TestEvictBySize_StopsAtThreshold(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// 3 files of 100 each = 300 total.
	seedCleanFile(t, cl, "a.txt", 100, 1)
	seedCleanFile(t, cl, "b.txt", 100, 2)
	seedCleanFile(t, cl, "c.txt", 100, 3)

	ev := &Evictor{MaxSize: 250} // need to evict just one (the oldest)
	ev.evictBySize(cl)

	a, _ := cl.db.GetFile("a.txt")
	assert.Equal(t, StateEvicted, a.State, "oldest should be evicted")

	b, _ := cl.db.GetFile("b.txt")
	assert.Equal(t, StateClean, b.State, "mid should be kept")

	c, _ := cl.db.GetFile("c.txt")
	assert.Equal(t, StateClean, c.State, "newest should be kept")
}

func TestEvictBySize_UnderLimit_Noop(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	seedCleanFile(t, cl, "small.txt", 50, 100)

	ev := &Evictor{MaxSize: 1000}
	ev.evictBySize(cl)

	entry, err := cl.db.GetFile("small.txt")
	require.NoError(t, err)
	assert.Equal(t, StateClean, entry.State)
}

func TestEvictBySize_ZeroMaxSize_Noop(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	seedCleanFile(t, cl, "keep.txt", 500, 100)

	ev := &Evictor{MaxSize: 0}
	ev.check(cl) // check should skip evictBySize since MaxSize == 0

	entry, err := cl.db.GetFile("keep.txt")
	require.NoError(t, err)
	assert.Equal(t, StateClean, entry.State)
}

// ---------- evictEntry edge case ----------

func TestEvictEntry_AlreadyDeleted(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// Insert DB entry but don't create disk file.
	require.NoError(t, cl.db.PutFile(&FileEntry{
		Path:  "ghost.txt",
		Size:  100,
		Mode:  0100644,
		State: StateClean,
	}))

	// Should not panic or error — os.IsNotExist is handled gracefully.
	evictEntry(cl, &FileEntry{Path: "ghost.txt", Size: 100})

	entry, err := cl.db.GetFile("ghost.txt")
	require.NoError(t, err)
	assert.Equal(t, StateEvicted, entry.State)
}

func TestEvictPath_HappyPath(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	seedCleanFile(t, cl, "a.txt", 123, time.Now().Unix())

	require.NoError(t, EvictPath(cl, "a.txt"))

	_, err := os.Stat(cl.diskPath("a.txt"))
	assert.True(t, os.IsNotExist(err))

	e, err := cl.db.GetFile("a.txt")
	require.NoError(t, err)
	assert.Equal(t, StateEvicted, e.State)
}

func TestEvictPath_Directory(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	require.NoError(t, cl.db.PutFile(&FileEntry{
		Path:  "folder",
		IsDir: true,
		Mode:  040755,
		State: StateClean,
	}))

	err := EvictPath(cl, "folder")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}

// ---------- Evictor.Run ----------

func TestEvictorRun_ContextCancel(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	ev := &Evictor{MaxSize: 1000}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ev.Run(ctx, cl)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Run exited — good.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestEvictorRun_TriggerChannel(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	pastAccess := time.Now().Add(-2 * time.Hour).Unix()
	seedCleanFile(t, cl, "trigger.txt", 100, pastAccess)

	triggerC := make(chan struct{}, 1)
	ev := &Evictor{MaxAge: 1 * time.Hour, TriggerC: triggerC}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		ev.Run(ctx, cl)
		close(done)
	}()

	// Send trigger — should cause an eviction pass.
	triggerC <- struct{}{}

	// Wait a bit for the pass to complete.
	require.Eventually(t, func() bool {
		entry, err := cl.db.GetFile("trigger.txt")
		return err == nil && entry != nil && entry.State == StateEvicted
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	<-done
}

func TestEvictorRun_ClosedChannel(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	triggerC := make(chan struct{})
	ev := &Evictor{TriggerC: triggerC}

	done := make(chan struct{})
	go func() {
		ev.Run(context.Background(), cl)
		close(done)
	}()

	close(triggerC)

	select {
	case <-done:
		// Run exited — good.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after channel close")
	}
}

// ---------- Eviction skips pinned & dirty files ----------

func TestEvictByAge_SkipsPinnedFile(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	pastAccess := time.Now().Add(-2 * time.Hour).Unix()
	seedCleanFile(t, cl, "pinned.txt", 100, pastAccess)
	require.NoError(t, cl.db.SetPinned("pinned.txt", true))

	ev := &Evictor{MaxAge: 1 * time.Hour}
	ev.evictByAge(cl)

	entry, err := cl.db.GetFile("pinned.txt")
	require.NoError(t, err)
	assert.Equal(t, StateClean, entry.State, "pinned file should not be evicted")
}

func TestEvictBySize_SkipsDirtyFile(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// Dirty files should never be returned by ListEvictable, so they won't be
	// evicted even if capped on size.
	dp := cl.diskPath("dirty.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(dp), 0755))
	os.WriteFile(dp, make([]byte, 500), 0644)
	require.NoError(t, cl.db.PutFile(&FileEntry{
		Path: "dirty.txt", Size: 500, Mode: 0100644, State: StateDirty,
		LastAccess: 1, CachePath: dp,
	}))

	seedCleanFile(t, cl, "clean.txt", 100, 10)

	ev := &Evictor{MaxSize: 200}
	ev.evictBySize(cl)

	dirty, err := cl.db.GetFile("dirty.txt")
	require.NoError(t, err)
	assert.Equal(t, StateDirty, dirty.State, "dirty file must not be evicted")
}
