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
	Pinned       bool
	LastAccess   int64
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
	// Wait for a lock to clear instead of failing immediately with SQLITE_BUSY.
	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
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
		statements := strings.SplitSeq(string(content), ";")
		for stmt := range statements {
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
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files WHERE path = ?`, path)

	e := &FileEntry{}
	err := row.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
		&e.RemoteMtime, &e.LocalMtime,
		&e.CachePath, &e.State, &e.CachedRanges,
		&e.SyncError, &e.RetryAfter, &e.Checksum,
		&e.Pinned, &e.LastAccess)
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
		                   cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		                   pinned, last_access)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			checksum      = excluded.checksum,
			pinned        = excluded.pinned,
			last_access   = excluded.last_access`,
		e.Path, e.IsDir, e.Size, e.Mode, e.RemoteMtime, e.LocalMtime,
		e.CachePath, e.State, e.CachedRanges, e.SyncError, e.RetryAfter, e.Checksum,
		e.Pinned, e.LastAccess)
	if err != nil {
		return fmt.Errorf("put file %q: %w", e.Path, err)
	}
	return nil
}

// PutFileTx inserts or updates a FileEntry inside an existing transaction.
func (m *MetadataDB) PutFileTx(tx *sql.Tx, e *FileEntry) error {
	_, err := tx.Exec(`
		INSERT INTO files (path, is_dir, size, mode, remote_mtime, local_mtime,
		                   cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		                   pinned, last_access)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			checksum      = excluded.checksum,
			pinned        = excluded.pinned,
			last_access   = excluded.last_access`,
		e.Path, e.IsDir, e.Size, e.Mode, e.RemoteMtime, e.LocalMtime,
		e.CachePath, e.State, e.CachedRanges, e.SyncError, e.RetryAfter, e.Checksum,
		e.Pinned, e.LastAccess)
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

// MarkEvicted resets cache-local transient metadata for path and marks it
// StateEvicted so subsequent opens trigger re-download.
func (m *MetadataDB) MarkEvicted(path string) error {
	res, err := m.db.Exec(`
		UPDATE files
		SET state = ?, cached_ranges = '', sync_error = '', retry_after = 0
		WHERE path = ?`, StateEvicted, path)
	if err != nil {
		return fmt.Errorf("mark evicted %q: %w", path, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("mark evicted: path %q not found", path)
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
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files
		WHERE path LIKE ? AND path NOT LIKE ?`,
		prefix+"%", prefix+"%/%")
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
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
			return nil, fmt.Errorf("scan dir entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListDescendants returns all entries under dirPath recursively.
// The returned entries do not include dirPath itself.
func (m *MetadataDB) ListDescendants(dirPath string) ([]*FileEntry, error) {
	prefix := dirPath + "/"

	rows, err := m.db.Query(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files
		WHERE path LIKE ?
		ORDER BY path`,
		prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("list descendants %q: %w", dirPath, err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
			return nil, fmt.Errorf("scan descendant entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListByState returns all entries with the given state.
func (m *MetadataDB) ListByState(state FileState) ([]*FileEntry, error) {
	rows, err := m.db.Query(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
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
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
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
	if op.Op == "put" {
		var existingID int64
		err := m.db.QueryRow(`SELECT id FROM pending_ops WHERE op = 'put' AND path = ? ORDER BY id LIMIT 1`, op.Path).Scan(&existingID)
		switch err {
		case nil:
			_, err = m.db.Exec(`
				UPDATE pending_ops
				SET queued_at = ?, attempts = 0, last_error = '', dest_path = ''
				WHERE id = ?`,
				op.QueuedAt, existingID)
			if err != nil {
				return fmt.Errorf("refresh pending put: %w", err)
			}
			return nil
		case sql.ErrNoRows:
		default:
			return fmt.Errorf("lookup pending put: %w", err)
		}
	}
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
	if op.Op == "put" {
		var existingID int64
		err := tx.QueryRow(`SELECT id FROM pending_ops WHERE op = 'put' AND path = ? ORDER BY id LIMIT 1`, op.Path).Scan(&existingID)
		switch err {
		case nil:
			_, err = tx.Exec(`
				UPDATE pending_ops
				SET queued_at = ?, attempts = 0, last_error = '', dest_path = ''
				WHERE id = ?`,
				op.QueuedAt, existingID)
			if err != nil {
				return fmt.Errorf("refresh pending put tx: %w", err)
			}
			return nil
		case sql.ErrNoRows:
		default:
			return fmt.Errorf("lookup pending put tx: %w", err)
		}
	}
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

// SetChecksum stores a checksum for the given path.
func (m *MetadataDB) SetChecksum(filePath, checksum string) error {
	_, err := m.db.Exec(`UPDATE files SET checksum = ? WHERE path = ?`, checksum, filePath)
	if err != nil {
		return fmt.Errorf("set checksum %q: %w", filePath, err)
	}
	return nil
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

// ---------- eviction helpers ----------

// SetPinned sets or clears the pinned flag for a file path.
func (m *MetadataDB) SetPinned(path string, pinned bool) error {
	v := 0
	if pinned {
		v = 1
	}
	res, err := m.db.Exec(`UPDATE files SET pinned = ? WHERE path = ?`, v, path)
	if err != nil {
		return fmt.Errorf("set pinned %q: %w", path, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("set pinned: path %q not found", path)
	}
	return nil
}

// SetPinnedMany sets or clears the pinned flag for multiple exact paths.
// The update is transactional: either all paths are updated or none are.
func (m *MetadataDB) SetPinnedMany(paths []string, pinned bool) error {
	if len(paths) == 0 {
		return nil
	}

	v := 0
	if pinned {
		v = 1
	}

	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("set pinned many: begin tx: %w", err)
	}

	stmt, err := tx.Prepare(`UPDATE files SET pinned = ? WHERE path = ?`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set pinned many: prepare: %w", err)
	}
	defer stmt.Close()

	for _, p := range paths {
		res, err := stmt.Exec(v, p)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set pinned many %q: %w", p, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			_ = tx.Rollback()
			return fmt.Errorf("set pinned many: path %q not found", p)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set pinned many: commit: %w", err)
	}
	return nil
}

// CheckpointRanges updates the cached_ranges column only when the row is still
// in StateDownloading. This is an atomic single-statement update so it cannot
// race with finish() or cancel() and accidentally overwrite a terminal state.
func (m *MetadataDB) CheckpointRanges(path, cachedRanges string) error {
	_, err := m.db.Exec(
		`UPDATE files SET cached_ranges = ? WHERE path = ? AND state = 'downloading'`,
		cachedRanges, path)
	if err != nil {
		return fmt.Errorf("checkpoint ranges %q: %w", path, err)
	}
	return nil
}

// UpdateLastAccess sets last_access to ts (Unix seconds) for the given path.
// Errors are silently ignored by callers — best-effort tracking.
func (m *MetadataDB) UpdateLastAccess(path string, ts int64) error {
	_, err := m.db.Exec(`UPDATE files SET last_access = ? WHERE path = ?`, ts, path)
	if err != nil {
		return fmt.Errorf("update last_access %q: %w", path, err)
	}
	return nil
}

// ListPinned returns all file entries that have pinned = 1.
func (m *MetadataDB) ListPinned() ([]*FileEntry, error) {
	rows, err := m.db.Query(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files WHERE pinned = 1 ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list pinned: %w", err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
			return nil, fmt.Errorf("scan pinned entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListEvictable returns clean, unpinned, non-directory entries ordered by
// last_access ascending (least recently accessed first). These are candidates
// for cache eviction.
func (m *MetadataDB) ListEvictable() ([]*FileEntry, error) {
	rows, err := m.db.Query(`
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files
		WHERE state = 'clean' AND pinned = 0 AND is_dir = 0
		ORDER BY last_access ASC`)
	if err != nil {
		return nil, fmt.Errorf("list evictable: %w", err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
			return nil, fmt.Errorf("scan evictable entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListCleanupCandidates returns non-directory entries that are safe to remove
// from local cache via the CLI cleanup command. It includes files in clean,
// downloading, or evicted states ordered by last_access ascending (least
// recently accessed first). When includePinned is false, pinned entries are
// excluded. When relPrefix is non-empty, results are scoped to relPrefix
// exactly and its descendants.
func (m *MetadataDB) ListCleanupCandidates(includePinned bool, relPrefix string) ([]*FileEntry, error) {
	query := `
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files
		WHERE (cached_ranges != '' OR state IN ('downloading', 'clean')) AND is_dir = 0`
	args := []any{}
	if !includePinned {
		query += ` AND pinned = 0`
	}
	if relPrefix != "" {
		query += ` AND (path = ? OR path LIKE ?)`
		args = append(args, relPrefix, relPrefix+"/%")
	}
	query += ` ORDER BY last_access ASC`

	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list cleanup candidates: %w", err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
			return nil, fmt.Errorf("scan cleanup candidate: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// ListCleanFiles is kept for backward compatibility with existing callers and
// tests that only need clean-state files.
func (m *MetadataDB) ListCleanFiles(includePinned bool) ([]*FileEntry, error) {
	query := `
		SELECT path, is_dir, size, mode, remote_mtime, local_mtime,
		       cache_path, state, cached_ranges, sync_error, retry_after, checksum,
		       pinned, last_access
		FROM files
		WHERE state = 'clean' AND is_dir = 0`
	if !includePinned {
		query += ` AND pinned = 0`
	}
	query += ` ORDER BY last_access ASC`

	rows, err := m.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("list clean files: %w", err)
	}
	defer rows.Close()

	var result []*FileEntry
	for rows.Next() {
		e := &FileEntry{}
		if err := rows.Scan(&e.Path, &e.IsDir, &e.Size, &e.Mode,
			&e.RemoteMtime, &e.LocalMtime,
			&e.CachePath, &e.State, &e.CachedRanges,
			&e.SyncError, &e.RetryAfter, &e.Checksum,
			&e.Pinned, &e.LastAccess); err != nil {
			return nil, fmt.Errorf("scan clean entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

// CountPendingOps returns the number of rows in pending_ops.
func (m *MetadataDB) CountPendingOps() (int, error) {
	var n int
	err := m.db.QueryRow(`SELECT COUNT(*) FROM pending_ops`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending ops: %w", err)
	}
	return n, nil
}

// CountConflicts returns the number of rows in the conflicts table.
func (m *MetadataDB) CountConflicts() (int, error) {
	var n int
	err := m.db.QueryRow(`SELECT COUNT(*) FROM conflicts`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count conflicts: %w", err)
	}
	return n, nil
}

// ResetRetryAfter sets retry_after = 0 for all pending_ops, clearing backoff
// timers so the next sync cycle retries everything immediately.
func (m *MetadataDB) ResetRetryAfter() error {
	_, err := m.db.Exec(`UPDATE pending_ops SET retry_after = 0`)
	if err != nil {
		return fmt.Errorf("reset retry_after: %w", err)
	}
	return nil
}
