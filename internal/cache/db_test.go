package cache

import (
	"testing"
	"time"
)

func openTestDB(t *testing.T) *MetadataDB {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPutGetFile(t *testing.T) {
	db := openTestDB(t)

	e := &FileEntry{
		Path:        "docs/readme.md",
		IsDir:       false,
		Size:        42,
		Mode:        0100644,
		RemoteMtime: 1000,
		LocalMtime:  1001,
		CachePath:   "/tmp/cache/files/docs/readme.md",
		State:       StateDirty,
	}
	if err := db.PutFile(e); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	got, err := db.GetFile("docs/readme.md")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got == nil {
		t.Fatal("GetFile returned nil")
	}
	if got.Size != 42 {
		t.Fatalf("size: got %d want 42", got.Size)
	}
	if got.State != StateDirty {
		t.Fatalf("state: got %q want %q", got.State, StateDirty)
	}
	if got.Mode != 0100644 {
		t.Fatalf("mode: got %o want 0100644", got.Mode)
	}
}

func TestGetFileNotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.GetFile("nonexistent")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent path")
	}
}

func TestPutFileUpsert(t *testing.T) {
	db := openTestDB(t)

	e := &FileEntry{Path: "a.txt", Size: 10, Mode: 0100644, State: StateClean}
	if err := db.PutFile(e); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	e.Size = 20
	e.State = StateDirty
	if err := db.PutFile(e); err != nil {
		t.Fatalf("PutFile update: %v", err)
	}

	got, err := db.GetFile("a.txt")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got.Size != 20 {
		t.Fatalf("size after upsert: got %d want 20", got.Size)
	}
	if got.State != StateDirty {
		t.Fatalf("state after upsert: got %q want %q", got.State, StateDirty)
	}
}

func TestSetState(t *testing.T) {
	db := openTestDB(t)

	if err := db.PutFile(&FileEntry{Path: "f.txt", State: StateDirty, Mode: 0100644}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetState("f.txt", StateClean); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	got, _ := db.GetFile("f.txt")
	if got.State != StateClean {
		t.Fatalf("state: got %q want %q", got.State, StateClean)
	}
}

func TestSetStateNotFound(t *testing.T) {
	db := openTestDB(t)
	if err := db.SetState("nope", StateClean); err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestListDir(t *testing.T) {
	db := openTestDB(t)

	entries := []*FileEntry{
		{Path: "a.txt", State: StateClean, Mode: 0100644},
		{Path: "sub", IsDir: true, State: StateClean, Mode: 040755},
		{Path: "sub/b.txt", State: StateClean, Mode: 0100644},
		{Path: "sub/deep", IsDir: true, State: StateClean, Mode: 040755},
		{Path: "sub/deep/c.txt", State: StateClean, Mode: 0100644},
	}
	for _, e := range entries {
		if err := db.PutFile(e); err != nil {
			t.Fatal(err)
		}
	}

	// Root children
	root, err := db.ListDir("")
	if err != nil {
		t.Fatalf("ListDir root: %v", err)
	}
	if len(root) != 2 { // a.txt and sub
		t.Fatalf("root children: got %d want 2", len(root))
	}

	// sub/ children
	sub, err := db.ListDir("sub")
	if err != nil {
		t.Fatalf("ListDir sub: %v", err)
	}
	if len(sub) != 2 { // b.txt and deep
		t.Fatalf("sub children: got %d want 2", len(sub))
	}

	// sub/deep/ children
	deep, err := db.ListDir("sub/deep")
	if err != nil {
		t.Fatalf("ListDir sub/deep: %v", err)
	}
	if len(deep) != 1 {
		t.Fatalf("sub/deep children: got %d want 1", len(deep))
	}
}

func TestListByState(t *testing.T) {
	db := openTestDB(t)

	for _, e := range []*FileEntry{
		{Path: "clean.txt", State: StateClean, Mode: 0100644},
		{Path: "dirty1.txt", State: StateDirty, Mode: 0100644},
		{Path: "dirty2.txt", State: StateDirty, Mode: 0100644},
	} {
		if err := db.PutFile(e); err != nil {
			t.Fatal(err)
		}
	}

	dirty, err := db.ListByState(StateDirty)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(dirty) != 2 {
		t.Fatalf("dirty count: got %d want 2", len(dirty))
	}
}

func TestDeleteFile(t *testing.T) {
	db := openTestDB(t)

	if err := db.PutFile(&FileEntry{Path: "gone.txt", State: StateClean, Mode: 0100644}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteFile("gone.txt"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	got, _ := db.GetFile("gone.txt")
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestPendingOpsOrdering(t *testing.T) {
	db := openTestDB(t)

	now := time.Now().Unix()
	ops := []*PendingOp{
		{Op: "put", Path: "a.txt", QueuedAt: now},
		{Op: "delete", Path: "b.txt", QueuedAt: now + 1},
		{Op: "mkdir", Path: "dir", QueuedAt: now + 2},
	}
	for _, o := range ops {
		if err := db.AddPendingOp(o); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.NextPendingOps(10)
	if err != nil {
		t.Fatalf("NextPendingOps: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("count: got %d want 3", len(got))
	}
	// Should be ordered by id (insertion order).
	if got[0].Op != "put" || got[1].Op != "delete" || got[2].Op != "mkdir" {
		t.Fatalf("order: %v", got)
	}

	// Complete the first op.
	if err := db.CompletePendingOp(got[0].ID); err != nil {
		t.Fatalf("CompletePendingOp: %v", err)
	}
	remaining, _ := db.NextPendingOps(10)
	if len(remaining) != 2 {
		t.Fatalf("remaining: got %d want 2", len(remaining))
	}
}

func TestPendingOpsLimit(t *testing.T) {
	db := openTestDB(t)

	now := time.Now().Unix()
	for i := range 5 {
		if err := db.AddPendingOp(&PendingOp{Op: "put", Path: "f" + string(rune('0'+i)), QueuedAt: now}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.NextPendingOps(2)
	if err != nil {
		t.Fatalf("NextPendingOps: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit: got %d want 2", len(got))
	}
}
