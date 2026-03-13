package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	t.Parallel()
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
	require.NoError(t, db.PutFile(e))

	got, err := db.GetFile("docs/readme.md")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(42), got.Size)
	assert.Equal(t, StateDirty, got.State)
	assert.Equal(t, uint32(0100644), got.Mode)
}

func TestGetFileNotFound(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	got, err := db.GetFile("nonexistent")
	require.NoError(t, err)
	assert.Nil(t, got, "expected nil for nonexistent path")
}

func TestPutFileUpsert(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	e := &FileEntry{Path: "a.txt", Size: 10, Mode: 0100644, State: StateClean}
	require.NoError(t, db.PutFile(e))

	e.Size = 20
	e.State = StateDirty
	require.NoError(t, db.PutFile(e))

	got, err := db.GetFile("a.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(20), got.Size)
	assert.Equal(t, StateDirty, got.State)
}

func TestSetState(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	require.NoError(t, db.PutFile(&FileEntry{Path: "f.txt", State: StateDirty, Mode: 0100644}))
	require.NoError(t, db.SetState("f.txt", StateClean))

	got, err := db.GetFile("f.txt")
	require.NoError(t, err)
	assert.Equal(t, StateClean, got.State)
}

func TestSetStateNotFound(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	assert.Error(t, db.SetState("nope", StateClean))
}

func TestListDir(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	entries := []*FileEntry{
		{Path: "a.txt", State: StateClean, Mode: 0100644},
		{Path: "sub", IsDir: true, State: StateClean, Mode: 040755},
		{Path: "sub/b.txt", State: StateClean, Mode: 0100644},
		{Path: "sub/deep", IsDir: true, State: StateClean, Mode: 040755},
		{Path: "sub/deep/c.txt", State: StateClean, Mode: 0100644},
	}
	for _, e := range entries {
		require.NoError(t, db.PutFile(e))
	}

	// Root children
	root, err := db.ListDir("")
	require.NoError(t, err)
	assert.Len(t, root, 2, "root should have a.txt and sub")

	// sub/ children
	sub, err := db.ListDir("sub")
	require.NoError(t, err)
	assert.Len(t, sub, 2, "sub should have b.txt and deep")

	// sub/deep/ children
	deep, err := db.ListDir("sub/deep")
	require.NoError(t, err)
	assert.Len(t, deep, 1, "sub/deep should have c.txt")
}

func TestListDescendants(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	entries := []*FileEntry{
		{Path: "a.txt", State: StateClean, Mode: 0100644},
		{Path: "sub", IsDir: true, State: StateClean, Mode: 040755},
		{Path: "sub/b.txt", State: StateClean, Mode: 0100644},
		{Path: "sub/deep", IsDir: true, State: StateClean, Mode: 040755},
		{Path: "sub/deep/c.txt", State: StateClean, Mode: 0100644},
		{Path: "sub2/x.txt", State: StateClean, Mode: 0100644},
	}
	for _, e := range entries {
		require.NoError(t, db.PutFile(e))
	}

	got, err := db.ListDescendants("sub")
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "sub/b.txt", got[0].Path)
	assert.Equal(t, "sub/deep", got[1].Path)
	assert.Equal(t, "sub/deep/c.txt", got[2].Path)
}

func TestListByState(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	for _, e := range []*FileEntry{
		{Path: "clean.txt", State: StateClean, Mode: 0100644},
		{Path: "dirty1.txt", State: StateDirty, Mode: 0100644},
		{Path: "dirty2.txt", State: StateDirty, Mode: 0100644},
	} {
		require.NoError(t, db.PutFile(e))
	}

	dirty, err := db.ListByState(StateDirty)
	require.NoError(t, err)
	assert.Len(t, dirty, 2)
}

func TestDeleteFile(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	require.NoError(t, db.PutFile(&FileEntry{Path: "gone.txt", State: StateClean, Mode: 0100644}))
	require.NoError(t, db.DeleteFile("gone.txt"))

	got, err := db.GetFile("gone.txt")
	require.NoError(t, err)
	assert.Nil(t, got, "expected nil after delete")
}

func TestSetPinnedMany(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	for _, p := range []string{"d", "d/a.txt", "d/sub", "d/sub/b.txt"} {
		isDir := p == "d" || p == "d/sub"
		mode := uint32(0100644)
		if isDir {
			mode = 040755
		}
		require.NoError(t, db.PutFile(&FileEntry{Path: p, IsDir: isDir, Mode: mode, State: StateClean}))
	}

	require.NoError(t, db.SetPinnedMany([]string{"d", "d/a.txt", "d/sub", "d/sub/b.txt"}, true))

	for _, p := range []string{"d", "d/a.txt", "d/sub", "d/sub/b.txt"} {
		e, err := db.GetFile(p)
		require.NoError(t, err)
		require.NotNil(t, e)
		assert.True(t, e.Pinned, "expected pinned for %s", p)
	}

	require.NoError(t, db.SetPinnedMany([]string{"d", "d/a.txt", "d/sub", "d/sub/b.txt"}, false))
	for _, p := range []string{"d", "d/a.txt", "d/sub", "d/sub/b.txt"} {
		e, err := db.GetFile(p)
		require.NoError(t, err)
		require.NotNil(t, e)
		assert.False(t, e.Pinned, "expected unpinned for %s", p)
	}
}

func TestSetPinnedManyRollbackOnMissingPath(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	require.NoError(t, db.PutFile(&FileEntry{Path: "a.txt", Mode: 0100644, State: StateClean}))

	err := db.SetPinnedMany([]string{"a.txt", "missing.txt"}, true)
	require.Error(t, err)

	e, getErr := db.GetFile("a.txt")
	require.NoError(t, getErr)
	require.NotNil(t, e)
	assert.False(t, e.Pinned, "update should be rolled back when one path is missing")
}

func TestPendingOpsOrdering(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	now := time.Now().Unix()
	ops := []*PendingOp{
		{Op: "put", Path: "a.txt", QueuedAt: now},
		{Op: "delete", Path: "b.txt", QueuedAt: now + 1},
		{Op: "mkdir", Path: "dir", QueuedAt: now + 2},
	}
	for _, o := range ops {
		require.NoError(t, db.AddPendingOp(o))
	}

	got, err := db.NextPendingOps(10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// Should be ordered by id (insertion order).
	assert.Equal(t, "put", got[0].Op)
	assert.Equal(t, "delete", got[1].Op)
	assert.Equal(t, "mkdir", got[2].Op)

	// Complete the first op.
	require.NoError(t, db.CompletePendingOp(got[0].ID))
	remaining, err := db.NextPendingOps(10)
	require.NoError(t, err)
	assert.Len(t, remaining, 2)
}

func TestPendingOpsLimit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	now := time.Now().Unix()
	for i := range 5 {
		require.NoError(t, db.AddPendingOp(&PendingOp{Op: "put", Path: "f" + string(rune('0'+i)), QueuedAt: now}))
	}

	got, err := db.NextPendingOps(2)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// ---------- HasFiles ----------

func TestHasFilesEmpty(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	has, err := db.HasFiles()
	require.NoError(t, err)
	assert.False(t, has)
}

func TestHasFilesNonEmpty(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.PutFile(&FileEntry{Path: "a.txt", State: StateClean, Mode: 0100644}))
	has, err := db.HasFiles()
	require.NoError(t, err)
	assert.True(t, has)
}

// ---------- CompletePendingOp ----------

func TestCompletePendingOp(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	now := time.Now().Unix()
	require.NoError(t, db.AddPendingOp(&PendingOp{Op: "put", Path: "a.txt", QueuedAt: now}))
	require.NoError(t, db.AddPendingOp(&PendingOp{Op: "delete", Path: "b.txt", QueuedAt: now}))

	ops, err := db.NextPendingOps(10)
	require.NoError(t, err)
	require.Len(t, ops, 2)

	require.NoError(t, db.CompletePendingOp(ops[0].ID))

	remaining, err := db.NextPendingOps(10)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "delete", remaining[0].Op)
}

// ---------- Transactions ----------

func TestBeginTxCommit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	tx, err := db.BeginTx()
	require.NoError(t, err)

	require.NoError(t, db.PutFileTx(tx, &FileEntry{
		Path: "tx-file.txt", State: StateDirty, Mode: 0100644,
	}))
	require.NoError(t, db.AddPendingOpTx(tx, &PendingOp{
		Op: "put", Path: "tx-file.txt", QueuedAt: time.Now().Unix(),
	}))
	require.NoError(t, tx.Commit())

	got, err := db.GetFile("tx-file.txt")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, StateDirty, got.State)
}

func TestBeginTxRollback(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	tx, err := db.BeginTx()
	require.NoError(t, err)

	require.NoError(t, db.PutFileTx(tx, &FileEntry{
		Path: "rollback.txt", State: StateDirty, Mode: 0100644,
	}))
	require.NoError(t, tx.Rollback())

	got, err := db.GetFile("rollback.txt")
	require.NoError(t, err)
	assert.Nil(t, got, "file should not exist after rollback")
}

// ---------- DrivePathEntry CRUD ----------

func TestPutGetDriveID(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.PutDriveID(&DrivePathEntry{
		Path: "docs/readme.md", DriveID: "drive-abc-123",
		ETag: "etag1", LastSeen: time.Now().Unix(),
	}))
	id, err := db.GetDriveID("docs/readme.md")
	require.NoError(t, err)
	assert.Equal(t, "drive-abc-123", id)
}

func TestGetDriveIDNotFound(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	id, err := db.GetDriveID("nonexistent")
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestPutDriveIDUpsert(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.PutDriveID(&DrivePathEntry{Path: "a.txt", DriveID: "old-id"}))
	require.NoError(t, db.PutDriveID(&DrivePathEntry{Path: "a.txt", DriveID: "new-id"}))
	id, err := db.GetDriveID("a.txt")
	require.NoError(t, err)
	assert.Equal(t, "new-id", id)
}

func TestDeleteDriveID(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.PutDriveID(&DrivePathEntry{Path: "rm.txt", DriveID: "id1"}))
	require.NoError(t, db.DeleteDriveID("rm.txt"))
	id, err := db.GetDriveID("rm.txt")
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestDeleteDriveIDsByPrefix(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	for _, p := range []string{"dir/a.txt", "dir/b.txt", "dir/sub/c.txt", "other.txt"} {
		require.NoError(t, db.PutDriveID(&DrivePathEntry{Path: p, DriveID: "id-" + p}))
	}
	require.NoError(t, db.DeleteDriveIDsByPrefix("dir"))

	for _, p := range []string{"dir/a.txt", "dir/b.txt", "dir/sub/c.txt"} {
		id, err := db.GetDriveID(p)
		require.NoError(t, err)
		assert.Empty(t, id, "expected %s to be deleted", p)
	}
	id, err := db.GetDriveID("other.txt")
	require.NoError(t, err)
	assert.Equal(t, "id-other.txt", id)
}

// ---------- Conflicts CRUD ----------

func TestAddListConflicts(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.AddConflict("a.txt", 100, 200))
	require.NoError(t, db.AddConflict("b.txt", 300, 400))

	conflicts, err := db.ListConflicts()
	require.NoError(t, err)
	require.Len(t, conflicts, 2)
	assert.Equal(t, "a.txt", conflicts[0].Path)
	assert.Equal(t, int64(100), conflicts[0].LocalMtime)
	assert.Equal(t, int64(200), conflicts[0].RemoteMtime)
	assert.Equal(t, "b.txt", conflicts[1].Path)
}

func TestGetConflict(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.AddConflict("test.txt", 100, 200))
	conflicts, _ := db.ListConflicts()
	require.Len(t, conflicts, 1)

	got, err := db.GetConflict(conflicts[0].ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "test.txt", got.Path)
}

func TestGetConflictNotFound(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	got, err := db.GetConflict(9999)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRemoveConflict(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.AddConflict("c.txt", 10, 20))
	conflicts, _ := db.ListConflicts()
	require.NoError(t, db.RemoveConflict(conflicts[0].ID))
	remaining, err := db.ListConflicts()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestRemoveConflictByPath(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.AddConflict("x.txt", 10, 20))
	require.NoError(t, db.AddConflict("y.txt", 30, 40))
	require.NoError(t, db.RemoveConflictByPath("x.txt"))
	conflicts, err := db.ListConflicts()
	require.NoError(t, err)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "y.txt", conflicts[0].Path)
}

func TestRemoveAllConflicts(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.AddConflict("a.txt", 1, 2))
	require.NoError(t, db.AddConflict("b.txt", 3, 4))
	require.NoError(t, db.RemoveAllConflicts())
	conflicts, err := db.ListConflicts()
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}

func TestAddConflictUpsert(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	require.NoError(t, db.AddConflict("f.txt", 10, 20))
	require.NoError(t, db.AddConflict("f.txt", 30, 40))
	conflicts, err := db.ListConflicts()
	require.NoError(t, err)
	require.Len(t, conflicts, 1, "upsert should not create duplicate")
	assert.Equal(t, int64(30), conflicts[0].LocalMtime)
	assert.Equal(t, int64(40), conflicts[0].RemoteMtime)
}

func TestListConflictsEmpty(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	conflicts, err := db.ListConflicts()
	require.NoError(t, err)
	assert.Empty(t, conflicts)
}
