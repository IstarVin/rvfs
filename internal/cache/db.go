package cache

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// FileState represents the synchronisation state of a cached file.
type FileState string

const (
	StateClean         FileState = "clean"
	StateDirty         FileState = "dirty"
	StateSyncing       FileState = "syncing"
	StateDownloading   FileState = "downloading"
	StateConflict      FileState = "conflict"
	StateDeletedLocal  FileState = "deleted_local"
	StateDeletedRemote FileState = "deleted_remote"
	StateEvicted       FileState = "evicted"
)

// FileEntry mirrors one row in the files table.
type FileEntry struct {
	Path         string
	IsDir        bool
	Size         int64
	Mode         uint32 // POSIX mode bits (including file-type)
	RemoteMtime  int64
	LocalMtime   int64
	CachePath    string
	State        FileState
	CachedRanges string
	SyncError    string
	RetryAfter   int64
	Checksum     string
}

// PendingOp mirrors one row in the pending_ops table.
type PendingOp struct {
	ID        int64
	Op        string
	Path      string
	DestPath  string
	QueuedAt  int64
	Attempts  int
	LastError string
}

// MetadataDB wraps a SQLite connection for the cache metadata store.
type MetadataDB struct {
	db *sql.DB
}

// OpenDB opens (or creates) the SQLite database at dbPath with WAL mode and
// applies the schema.
func OpenDB(dbPath string) (*MetadataDB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// WAL mode for concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set foreign_keys: %w", err)
	}

	if err := applySchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &MetadataDB{db: db}, nil
}

// Close closes the underlying database connection.
func (m *MetadataDB) Close() error {
	return m.db.Close()
}

// HasFiles reports whether the files table contains at least one row.
// It is used on startup to decide whether an initial remote pull is needed.
func (m *MetadataDB) HasFiles() (bool, error) {
	var n int
	err := m.db.QueryRow(`SELECT COUNT(*) FROM files LIMIT 1`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("has files: %w", err)
	}
	return n > 0, nil
}

func applySchema(db *sql.DB) error {
	// Read all migration files from the embedded migrations directory, sorted by name
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(files)

	for _, filename := range files {
		content, err := fs.ReadFile(migrationsFS, filename)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", filename, err)
		}

		// Split by semicolon to handle multiple statements per file
		statements := strings.Split(string(content), ";")
		for _, stmt := range statements {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("apply migration %s: %w", path.Base(filename), err)
			}
		}
	}
	return nil
}

// ---------- files CRUD ----------

// GetFile returns the FileEntry for path, or nil if not found.
func (m *MetadataDB) GetFile(path string) (*FileEntry, error) {
	row := m.db.QueryRow(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum
		FROM files WHERE path = ?`, path)

	e := &FileEntry{}
	err := row.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
		&e.RemoteMtime, &e.LocalMtime,
		&e.CachePath, &e.State, &e.CachedRanges,
		&e.SyncError, &e.RetryAfter, &e.Checksum)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get file %q: %w", path, err)
	}
	return e, nil
}

// PutFile inserts or updates a FileEntry (upsert).
func (m *MetadataDB) PutFile(e *FileEntry) error {
	_, err := m.db.Exec(`
		INSERT INTO files (path, is_dir, size, mode, remote_mtime, local_mtime,
		                   cache_path, state, cached_ranges, sync_error, retry_after, checksum)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			is_dir        = excluded.is_dir,
			size          = excluded.size,
			mode          = excluded.mode,
			remote_mtime  = excluded.remote_mtime,
			local_mtime   = excluded.local_mtime,
			cache_path    = excluded.cache_path,
			state         = excluded.state,
			cached_ranges = excluded.cached_ranges,
			sync_error    = excluded.sync_error,
			retry_after   = excluded.retry_after,
			checksum      = excluded.checksum`,
		e.Path, e.IsDir, e.Size, e.Mode, e.RemoteMtime, e.LocalMtime,
		e.CachePath, e.State, e.CachedRanges, e.SyncError, e.RetryAfter, e.Checksum)
	if err != nil {
		return fmt.Errorf("put file %q: %w", e.Path, err)
	}
	return nil
}

// PutFileTx inserts or updates a FileEntry inside an existing transaction.
func (m *MetadataDB) PutFileTx(tx *sql.Tx, e *FileEntry) error {
	_, err := tx.Exec(`
		INSERT INTO files (path, is_dir, size, mode, remote_mtime, local_mtime,
		                   cache_path, state, cached_ranges, sync_error, retry_after, checksum)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			is_dir        = excluded.is_dir,
			size          = excluded.size,
			mode          = excluded.mode,
			remote_mtime  = excluded.remote_mtime,
			local_mtime   = excluded.local_mtime,
			cache_path    = excluded.cache_path,
			state         = excluded.state,
			cached_ranges = excluded.cached_ranges,
			sync_error    = excluded.sync_error,
			retry_after   = excluded.retry_after,
			checksum      = excluded.checksum`,
		e.Path, e.IsDir, e.Size, e.Mode, e.RemoteMtime, e.LocalMtime,
		e.CachePath, e.State, e.CachedRanges, e.SyncError, e.RetryAfter, e.Checksum)
	if err != nil {
		return fmt.Errorf("put file tx %q: %w", e.Path, err)
	}
	return nil
}

// SetState updates the state column for the given path.
func (m *MetadataDB) SetState(path string, state FileState) error {
	res, err := m.db.Exec(`UPDATE files SET state = ? WHERE path = ?`, state, path)
	if err != nil {
		return fmt.Errorf("set state %q: %w", path, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("set state: path %q not found", path)
	}
	return nil
}

// ListDir returns the immediate children of dirPath.
// dirPath should be "" for the root directory.
func (m *MetadataDB) ListDir(dirPath string) ([]*FileEntry, error) {
	var prefix string
	if dirPath == "" {
		prefix = ""
	} else {
		prefix = dirPath + "/"
	}

	rows, err := m.db.Query(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum
		FROM files
		WHERE path LIKE ? AND path != ?`,
		prefix+"%", dirPath)
	if err != nil {
		return nil, fmt.Errorf("list dir %q: %w", dirPath, err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum); err != nil {
			return nil, fmt.Errorf("scan dir entry: %w", err)
		}
		// Filter to immediate children only: the relative portion after the
		// prefix must not contain a "/".
		rel := strings.TrimPrefix(e.Path, prefix)
		if strings.Contains(rel, "/") {
			continue
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListByState returns all entries with the given state.
func (m *MetadataDB) ListByState(state FileState) ([]*FileEntry, error) {
	rows, err := m.db.Query(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum
		FROM files WHERE state = ?`, state)
	if err != nil {
		return nil, fmt.Errorf("list by state %q: %w", state, err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum); err != nil {
			return nil, fmt.Errorf("scan state entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// DeleteFile removes a row from the files table.
func (m *MetadataDB) DeleteFile(path string) error {
	_, err := m.db.Exec(`DELETE FROM files WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("delete file %q: %w", path, err)
	}
	return nil
}

// ---------- pending_ops CRUD ----------

// AddPendingOp inserts a new pending operation.
func (m *MetadataDB) AddPendingOp(op *PendingOp) error {
	_, err := m.db.Exec(`
		INSERT INTO pending_ops (op, path, dest_path, queued_at, attempts, last_error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		op.Op, op.Path, op.DestPath, op.QueuedAt, op.Attempts, op.LastError)
	if err != nil {
		return fmt.Errorf("add pending op: %w", err)
	}
	return nil
}

// AddPendingOpTx inserts a new pending operation inside an existing transaction.
func (m *MetadataDB) AddPendingOpTx(tx *sql.Tx, op *PendingOp) error {
	_, err := tx.Exec(`
		INSERT INTO pending_ops (op, path, dest_path, queued_at, attempts, last_error)
		VALUES (?, ?, ?, ?, ?, ?)`,
		op.Op, op.Path, op.DestPath, op.QueuedAt, op.Attempts, op.LastError)
	if err != nil {
		return fmt.Errorf("add pending op tx: %w", err)
	}
	return nil
}

// NextPendingOps returns up to limit pending operations ordered by id.
func (m *MetadataDB) NextPendingOps(limit int) ([]*PendingOp, error) {
	rows, err := m.db.Query(`
		SELECT id, op, path, dest_path, queued_at, attempts, last_error
		FROM pending_ops ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("next pending ops: %w", err)
	}
	defer rows.Close()

	var result []*PendingOp
	for rows.Next() {
		o := &PendingOp{}
		if err := rows.Scan(&o.ID, &o.Op, &o.Path, &o.DestPath,
			&o.QueuedAt, &o.Attempts, &o.LastError); err != nil {
			return nil, fmt.Errorf("scan pending op: %w", err)
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

// CompletePendingOp deletes a pending operation by id.
func (m *MetadataDB) CompletePendingOp(id int64) error {
	_, err := m.db.Exec(`DELETE FROM pending_ops WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("complete pending op %d: %w", id, err)
	}
	return nil
}

// BeginTx starts a new transaction.
func (m *MetadataDB) BeginTx() (*sql.Tx, error) {
	return m.db.Begin()
}

// ---------- drive_path_ids CRUD ----------

// DrivePathEntry mirrors one row in the drive_path_ids table.
type DrivePathEntry struct {
	Path     string
	DriveID  string
	ETag     string
	LastSeen int64
}

// GetDriveID returns the Drive file ID for the given path, or "" if not cached.
func (m *MetadataDB) GetDriveID(path string) (string, error) {
	var id string
	err := m.db.QueryRow(`SELECT drive_id FROM drive_path_ids WHERE path = ?`, path).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get drive id %q: %w", path, err)
	}
	return id, nil
}

// PutDriveID inserts or updates a path→driveID mapping.
func (m *MetadataDB) PutDriveID(e *DrivePathEntry) error {
	_, err := m.db.Exec(`
		INSERT INTO drive_path_ids (path, drive_id, etag, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			drive_id  = excluded.drive_id,
			etag      = excluded.etag,
			last_seen = excluded.last_seen`,
		e.Path, e.DriveID, e.ETag, e.LastSeen)
	if err != nil {
		return fmt.Errorf("put drive id %q: %w", e.Path, err)
	}
	return nil
}

// DeleteDriveID removes the path→driveID mapping for the given path.
func (m *MetadataDB) DeleteDriveID(path string) error {
	_, err := m.db.Exec(`DELETE FROM drive_path_ids WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("delete drive id %q: %w", path, err)
	}
	return nil
}

// DeleteDriveIDsByPrefix removes all path→driveID mappings under prefix/.
func (m *MetadataDB) DeleteDriveIDsByPrefix(prefix string) error {
	_, err := m.db.Exec(`DELETE FROM drive_path_ids WHERE path LIKE ?`, prefix+"/%")
	if err != nil {
		return fmt.Errorf("delete drive ids prefix %q: %w", prefix, err)
	}
	return nil
}

// ---------- conflicts CRUD ----------

// ConflictEntry mirrors one row in the conflicts table.
type ConflictEntry struct {
	ID          int64
	Path        string
	LocalMtime  int64
	RemoteMtime int64
	DetectedAt  int64
}

// AddConflict inserts or updates a conflict record for path.
func (m *MetadataDB) AddConflict(path string, localMtime, remoteMtime int64) error {
	now := time.Now().Unix()
	_, err := m.db.Exec(`
		INSERT INTO conflicts (path, local_mtime, remote_mtime, detected_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			local_mtime  = excluded.local_mtime,
			remote_mtime = excluded.remote_mtime,
			detected_at  = excluded.detected_at`,
		path, localMtime, remoteMtime, now)
	if err != nil {
		return fmt.Errorf("add conflict %q: %w", path, err)
	}
	return nil
}

// RemoveConflict deletes a conflict record by ID.
func (m *MetadataDB) RemoveConflict(id int64) error {
	_, err := m.db.Exec(`DELETE FROM conflicts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove conflict %d: %w", id, err)
	}
	return nil
}

// RemoveConflictByPath deletes the conflict record for path.
func (m *MetadataDB) RemoveConflictByPath(path string) error {
	_, err := m.db.Exec(`DELETE FROM conflicts WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("remove conflict %q: %w", path, err)
	}
	return nil
}

// RemoveAllConflicts deletes all conflict records.
func (m *MetadataDB) RemoveAllConflicts() error {
	_, err := m.db.Exec(`DELETE FROM conflicts`)
	if err != nil {
		return fmt.Errorf("remove all conflicts: %w", err)
	}
	return nil
}

// ListConflicts returns all conflict records ordered by detected_at.
func (m *MetadataDB) ListConflicts() ([]*ConflictEntry, error) {
	rows, err := m.db.Query(`
		SELECT id, path, local_mtime, remote_mtime, detected_at
		FROM conflicts ORDER BY detected_at`)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	defer rows.Close()

	var result []*ConflictEntry
	for rows.Next() {
		e := &ConflictEntry{}
		if err := rows.Scan(&e.ID, &e.Path, &e.LocalMtime, &e.RemoteMtime, &e.DetectedAt); err != nil {
			return nil, fmt.Errorf("scan conflict: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// GetConflict returns the conflict record with the given ID, or nil if not found.
func (m *MetadataDB) GetConflict(id int64) (*ConflictEntry, error) {
	e := &ConflictEntry{}
	err := m.db.QueryRow(`
		SELECT id, path, local_mtime, remote_mtime, detected_at
		FROM conflicts WHERE id = ?`, id).Scan(
		&e.ID, &e.Path, &e.LocalMtime, &e.RemoteMtime, &e.DetectedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get conflict %d: %w", id, err)
	}
	return e, nil
}
