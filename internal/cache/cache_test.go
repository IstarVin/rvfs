package cache

import (
	"os"
	"path/filepath"
	"testing"
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
