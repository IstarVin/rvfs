package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCache(t *testing.T) *CacheLayer {
	t.Helper()
	cacheBase := t.TempDir()
	cl, err := NewCacheLayer(cacheBase, "test-remote")
	if err != nil {
		t.Fatalf("NewCacheLayer: %v", err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

func TestCacheCreateStat(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, entry, err := cl.Create("hello.txt", 0644)
	require.NoError(t, err)
	f.Close()

	assert.Equal(t, StateDirty, entry.State)

	got, err := cl.Stat("hello.txt")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StateDirty, got.State)
}

func TestCacheWriteRead(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("data.txt", 0644)
	require.NoError(t, err)
	f.Close()

	data := []byte("hello, cache!")
	n, err := cl.Write("data.txt", data, 0)
	require.NoError(t, err)
	assert.Equal(t, len(data), n)

	buf := make([]byte, 100)
	nr, err := cl.Read("data.txt", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, "hello, cache!", string(buf[:nr]))
}

func TestCacheDelete(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("gone.txt", 0644)
	require.NoError(t, err)
	f.Close()

	require.NoError(t, cl.Delete("gone.txt"))

	got, err := cl.Stat("gone.txt")
	require.NoError(t, err)
	assert.Nil(t, got, "expected nil after delete")

	// Verify pending_ops has the delete.
	ops, err := cl.DB().NextPendingOps(10)
	require.NoError(t, err)
	found := false
	for _, o := range ops {
		if o.Op == "delete" && o.Path == "gone.txt" {
			found = true
		}
	}
	assert.True(t, found, "no delete pending op found")
}

func TestCacheMkdirRmdir(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	entry, err := cl.Mkdir("subdir", 0755)
	require.NoError(t, err)
	assert.True(t, entry.IsDir, "entry should be a directory")
	assert.Equal(t, StateDirty, entry.State)

	require.NoError(t, cl.Rmdir("subdir"))

	got, _ := cl.Stat("subdir")
	assert.Nil(t, got, "expected nil after rmdir")
}

func TestCacheRename(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("old.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("content"))
	f.Close()

	require.NoError(t, cl.Rename("old.txt", "new.txt"))

	old, _ := cl.Stat("old.txt")
	assert.Nil(t, old, "old path should not exist")

	got, err := cl.Stat("new.txt")
	require.NoError(t, err)
	assert.NotNil(t, got, "new path should exist")

	// Verify pending_ops has a rename entry.
	ops, _ := cl.DB().NextPendingOps(10)
	found := false
	for _, o := range ops {
		if o.Op == "rename" && o.Path == "old.txt" && o.DestPath == "new.txt" {
			found = true
		}
	}
	assert.True(t, found, "no rename pending op found")
}

func TestCacheTruncate(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("trunc.txt", 0644)
	require.NoError(t, err)
	f.Write([]byte("hello world"))
	f.Close()

	// Write to update the size in the DB.
	cl.Write("trunc.txt", []byte("hello world"), 0)

	require.NoError(t, cl.Truncate("trunc.txt", 5))

	got, _ := cl.Stat("trunc.txt")
	assert.Equal(t, int64(5), got.Size)
}

func TestCacheReadDir(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	cl.Mkdir("dir", 0755)
	f1, _, _ := cl.Create("dir/a.txt", 0644)
	f1.Close()
	f2, _, _ := cl.Create("dir/b.txt", 0644)
	f2.Close()

	entries, err := cl.ReadDir("dir")
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestCacheSeedFromDir(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// Create a source directory to seed from.
	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0755)
	os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root"), 0644)
	os.WriteFile(filepath.Join(srcDir, "sub", "child.txt"), []byte("child"), 0644)

	require.NoError(t, cl.SeedFromDir(srcDir))

	// Verify entries.
	root, err := cl.Stat("root.txt")
	require.NoError(t, err)
	require.NotNil(t, root, "root.txt not found")
	assert.Equal(t, StateClean, root.State)
	assert.Equal(t, int64(4), root.Size)

	child, err := cl.Stat("sub/child.txt")
	require.NoError(t, err)
	assert.NotNil(t, child, "sub/child.txt not found")

	sub, err := cl.Stat("sub")
	require.NoError(t, err)
	require.NotNil(t, sub)
	assert.True(t, sub.IsDir, "sub should be a dir entry")

	// ReadDir on root should find 2 entries.
	entries, err := cl.ReadDir("")
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestCachePendingOpsChronological(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// Create, then write, then rename — ops should be in order.
	f, _, _ := cl.Create("seq.txt", 0644)
	f.Close()
	cl.Write("seq.txt", []byte("data"), 0)
	cl.Rename("seq.txt", "seq2.txt")

	ops, err := cl.DB().NextPendingOps(10)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ops), 3)

	// Verify chronological: create(put) -> write(put) -> rename
	assert.Equal(t, "put", ops[0].Op)
	assert.Equal(t, "rename", ops[len(ops)-1].Op)
	// IDs should be strictly increasing.
	for i := 1; i < len(ops); i++ {
		assert.Greater(t, ops[i].ID, ops[i-1].ID, "ops not in order at index %d", i)
	}
}

// ---------- Extended tests ----------

func TestCacheChmod(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("chmod.txt", 0644)
	require.NoError(t, err)
	f.Close()

	require.NoError(t, cl.Chmod("chmod.txt", 0755))

	entry, err := cl.Stat("chmod.txt")
	require.NoError(t, err)
	// The POSIX mode should include the new permission bits.
	assert.Equal(t, os.FileMode(0755), os.FileMode(entry.Mode).Perm())
}

func TestCacheChtimes(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("times.txt", 0644)
	require.NoError(t, err)
	f.Close()

	now := time.Now().Truncate(time.Second)
	require.NoError(t, cl.Chtimes("times.txt", now, now))

	entry, err := cl.Stat("times.txt")
	require.NoError(t, err)
	assert.Equal(t, now.Unix(), entry.LocalMtime)
}

func TestCacheDeleteNonexistent(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)
	// Deleting a path that was never created should succeed silently —
	// the in-flight download or upload may have already removed the file.
	err := cl.Delete("no-such-file.txt")
	assert.NoError(t, err)
}

func TestCacheRenameNonexistent(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)
	err := cl.Rename("no-src.txt", "dst.txt")
	assert.Error(t, err)
}

func TestCacheReadDirEmpty(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)
	_, err := cl.Mkdir("empty", 0755)
	require.NoError(t, err)

	entries, err := cl.ReadDir("empty")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestCacheWriteAtOffset(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("offset.txt", 0644)
	require.NoError(t, err)
	// Pre-fill with zeros.
	f.Write(make([]byte, 20))
	f.Close()
	cl.Write("offset.txt", make([]byte, 20), 0)

	// Write at offset 10.
	data := []byte("HELLO")
	n, err := cl.Write("offset.txt", data, 10)
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	buf := make([]byte, 20)
	nr, err := cl.Read("offset.txt", buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 20, nr)
	assert.Equal(t, "HELLO", string(buf[10:15]))
}

func TestCacheSeedFromDirNested(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "a", "b", "c"), 0755)
	os.WriteFile(filepath.Join(srcDir, "a", "b", "c", "deep.txt"), []byte("deep"), 0644)
	os.WriteFile(filepath.Join(srcDir, "a", "top.txt"), []byte("top"), 0644)

	require.NoError(t, cl.SeedFromDir(srcDir))

	for _, p := range []string{"a", "a/b", "a/b/c", "a/b/c/deep.txt", "a/top.txt"} {
		got, err := cl.Stat(p)
		require.NoError(t, err, "path: %s", p)
		assert.NotNil(t, got, "expected entry for %s", p)
	}
}

func TestCacheSeedFromDirEmpty(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)
	srcDir := t.TempDir()

	require.NoError(t, cl.SeedFromDir(srcDir))

	entries, err := cl.ReadDir("")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestCacheOpenOrCreate(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	// Create new file.
	f, err := cl.OpenOrCreate("new.bin", 1024)
	require.NoError(t, err)
	info, _ := f.Stat()
	assert.Equal(t, int64(1024), info.Size())
	f.Close()

	// Reopen — should reuse existing file of same size.
	f2, err := cl.OpenOrCreate("new.bin", 1024)
	require.NoError(t, err)
	f2.Close()
}

func TestCacheDiskPath(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)
	p := cl.DiskPath("sub/file.txt")
	assert.Contains(t, p, "files")
	assert.Contains(t, p, "sub/file.txt")
}

func TestCacheLstatDisk(t *testing.T) {
	t.Parallel()
	cl := newTestCache(t)

	f, _, err := cl.Create("stat.txt", 0644)
	require.NoError(t, err)
	f.Close()

	st, err := cl.LstatDisk("stat.txt")
	require.NoError(t, err)
	assert.NotNil(t, st)
}
