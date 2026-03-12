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
	cl := newTestCache(t)

	f, entry, err := cl.Create("hello.txt", 0644)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Close()

	if entry.State != StateDirty {
		t.Fatalf("state: got %q want %q", entry.State, StateDirty)
	}

	got, err := cl.Stat("hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got == nil {
		t.Fatal("Stat returned nil")
	}
	if got.State != StateDirty {
		t.Fatalf("state: got %q want %q", got.State, StateDirty)
	}
}

func TestCacheWriteRead(t *testing.T) {
	cl := newTestCache(t)

	f, _, err := cl.Create("data.txt", 0644)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Close()

	data := []byte("hello, cache!")
	n, err := cl.Write("data.txt", data, 0)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("Write n: got %d want %d", n, len(data))
	}

	buf := make([]byte, 100)
	nr, err := cl.Read("data.txt", buf, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:nr]) != "hello, cache!" {
		t.Fatalf("content: got %q", buf[:nr])
	}
}

func TestCacheDelete(t *testing.T) {
	cl := newTestCache(t)

	f, _, err := cl.Create("gone.txt", 0644)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Close()

	if err := cl.Delete("gone.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := cl.Stat("gone.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil after delete")
	}

	// Verify pending_ops has the delete.
	ops, err := cl.DB().NextPendingOps(10)
	if err != nil {
		t.Fatalf("NextPendingOps: %v", err)
	}
	found := false
	for _, o := range ops {
		if o.Op == "delete" && o.Path == "gone.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("no delete pending op found")
	}
}

func TestCacheMkdirRmdir(t *testing.T) {
	cl := newTestCache(t)

	entry, err := cl.Mkdir("subdir", 0755)
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if !entry.IsDir {
		t.Fatal("entry should be a directory")
	}
	if entry.State != StateDirty {
		t.Fatalf("state: got %q want %q", entry.State, StateDirty)
	}

	if err := cl.Rmdir("subdir"); err != nil {
		t.Fatalf("Rmdir: %v", err)
	}

	got, _ := cl.Stat("subdir")
	if got != nil {
		t.Fatal("expected nil after rmdir")
	}
}

func TestCacheRename(t *testing.T) {
	cl := newTestCache(t)

	f, _, err := cl.Create("old.txt", 0644)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Write([]byte("content"))
	f.Close()

	if err := cl.Rename("old.txt", "new.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	old, _ := cl.Stat("old.txt")
	if old != nil {
		t.Fatal("old path should not exist")
	}

	got, err := cl.Stat("new.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got == nil {
		t.Fatal("new path should exist")
	}

	// Verify pending_ops has a rename entry.
	ops, _ := cl.DB().NextPendingOps(10)
	found := false
	for _, o := range ops {
		if o.Op == "rename" && o.Path == "old.txt" && o.DestPath == "new.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("no rename pending op found")
	}
}

func TestCacheTruncate(t *testing.T) {
	cl := newTestCache(t)

	f, _, err := cl.Create("trunc.txt", 0644)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Write([]byte("hello world"))
	f.Close()

	// Write to update the size in the DB.
	cl.Write("trunc.txt", []byte("hello world"), 0)

	if err := cl.Truncate("trunc.txt", 5); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	got, _ := cl.Stat("trunc.txt")
	if got.Size != 5 {
		t.Fatalf("size: got %d want 5", got.Size)
	}
}

func TestCacheReadDir(t *testing.T) {
	cl := newTestCache(t)

	cl.Mkdir("dir", 0755)
	f1, _, _ := cl.Create("dir/a.txt", 0644)
	f1.Close()
	f2, _, _ := cl.Create("dir/b.txt", 0644)
	f2.Close()

	entries, err := cl.ReadDir("dir")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ReadDir: got %d want 2", len(entries))
	}
}

func TestCacheSeedFromDir(t *testing.T) {
	cl := newTestCache(t)

	// Create a source directory to seed from.
	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0755)
	os.WriteFile(filepath.Join(srcDir, "root.txt"), []byte("root"), 0644)
	os.WriteFile(filepath.Join(srcDir, "sub", "child.txt"), []byte("child"), 0644)

	if err := cl.SeedFromDir(srcDir); err != nil {
		t.Fatalf("SeedFromDir: %v", err)
	}

	// Verify entries.
	root, _ := cl.Stat("root.txt")
	if root == nil {
		t.Fatal("root.txt not found")
	}
	if root.State != StateClean {
		t.Fatalf("root.txt state: got %q want %q", root.State, StateClean)
	}
	if root.Size != 4 {
		t.Fatalf("root.txt size: got %d want 4", root.Size)
	}

	child, _ := cl.Stat("sub/child.txt")
	if child == nil {
		t.Fatal("sub/child.txt not found")
	}

	sub, _ := cl.Stat("sub")
	if sub == nil || !sub.IsDir {
		t.Fatal("sub should be a dir entry")
	}

	// ReadDir on root should find 2 entries.
	entries, _ := cl.ReadDir("")
	if len(entries) != 2 {
		t.Fatalf("root ReadDir: got %d want 2", len(entries))
	}
}

func TestCachePendingOpsChronological(t *testing.T) {
	cl := newTestCache(t)

	// Create, then write, then rename — ops should be in order.
	f, _, _ := cl.Create("seq.txt", 0644)
	f.Close()
	cl.Write("seq.txt", []byte("data"), 0)
	cl.Rename("seq.txt", "seq2.txt")

	ops, err := cl.DB().NextPendingOps(10)
	if err != nil {
		t.Fatalf("NextPendingOps: %v", err)
	}
	if len(ops) < 3 {
		t.Fatalf("expected at least 3 ops, got %d", len(ops))
	}

	// Verify chronological: create(put) -> write(put) -> rename
	if ops[0].Op != "put" {
		t.Fatalf("op[0]: got %q want put", ops[0].Op)
	}
	if ops[len(ops)-1].Op != "rename" {
		t.Fatalf("last op: got %q want rename", ops[len(ops)-1].Op)
	}
	// IDs should be strictly increasing.
	for i := 1; i < len(ops); i++ {
		if ops[i].ID <= ops[i-1].ID {
			t.Fatalf("ops not in order: id[%d]=%d <= id[%d]=%d", i, ops[i].ID, i-1, ops[i-1].ID)
		}
	}
}

// ---------- Extended tests ----------

func TestCacheChmod(t *testing.T) {
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
	cl := newTestCache(t)
	err := cl.Delete("no-such-file.txt")
	assert.Error(t, err)
}

func TestCacheRenameNonexistent(t *testing.T) {
	cl := newTestCache(t)
	err := cl.Rename("no-src.txt", "dst.txt")
	assert.Error(t, err)
}

func TestCacheReadDirEmpty(t *testing.T) {
	cl := newTestCache(t)
	_, err := cl.Mkdir("empty", 0755)
	require.NoError(t, err)

	entries, err := cl.ReadDir("empty")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestCacheWriteAtOffset(t *testing.T) {
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
	cl := newTestCache(t)
	srcDir := t.TempDir()

	require.NoError(t, cl.SeedFromDir(srcDir))

	entries, err := cl.ReadDir("")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestCacheOpenOrCreate(t *testing.T) {
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
	cl := newTestCache(t)
	p := cl.DiskPath("sub/file.txt")
	assert.Contains(t, p, "files")
	assert.Contains(t, p, "sub/file.txt")
}

func TestCacheLstatDisk(t *testing.T) {
	cl := newTestCache(t)

	f, _, err := cl.Create("stat.txt", 0644)
	require.NoError(t, err)
	f.Close()

	st, err := cl.LstatDisk("stat.txt")
	require.NoError(t, err)
	assert.NotNil(t, st)
}
