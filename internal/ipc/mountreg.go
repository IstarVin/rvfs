package ipc

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
)

// MountEntry describes one active (or recently-active) mount.
type MountEntry struct {
	Mountpoint string
	Source     string
	RemoteName string
	SockPath   string
	PID        int
	MountedAt  int64
}

// MountRegistry is a lightweight SQLite database that tracks running mounts.
// It lives at MountRegPath() so it is visible to all processes of the same user.
type MountRegistry struct {
	db *sql.DB
}

const mountSchema = `
CREATE TABLE IF NOT EXISTS mounts (
    mountpoint  TEXT PRIMARY KEY,
    source      TEXT NOT NULL,
    remote_name TEXT NOT NULL,
    sock_path   TEXT NOT NULL,
    pid         INTEGER NOT NULL DEFAULT 0,
    mounted_at  INTEGER NOT NULL DEFAULT 0
);`

// OpenMountRegistry opens (or creates) the mount registry database.
func OpenMountRegistry() (*MountRegistry, error) {
	return openMountRegistryAt(MountRegPath())
}

// openMountRegistryAt opens (or creates) the mount registry database at the
// given path. The directory is created if it does not exist.
func openMountRegistryAt(regPath string) (*MountRegistry, error) {
	if err := os.MkdirAll(filepath.Dir(regPath), 0700); err != nil {
		return nil, fmt.Errorf("mountreg: mkdir: %w", err)
	}
	db, err := sql.Open("sqlite", regPath+"?_journal=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("mountreg: open: %w", err)
	}
	if _, err := db.Exec(mountSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("mountreg: schema: %w", err)
	}
	return &MountRegistry{db: db}, nil
}

// Close closes the underlying database.
func (r *MountRegistry) Close() error { return r.db.Close() }

// Register inserts or replaces an entry for the given mountpoint.
func (r *MountRegistry) Register(e MountEntry) error {
	_, err := r.db.Exec(
		`INSERT OR REPLACE INTO mounts
		 (mountpoint, source, remote_name, sock_path, pid, mounted_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.Mountpoint, e.Source, e.RemoteName, e.SockPath, e.PID, e.MountedAt,
	)
	return err
}

// Deregister removes the entry for a mountpoint. It is a no-op if the
// mountpoint is not registered.
func (r *MountRegistry) Deregister(mountpoint string) error {
	_, err := r.db.Exec(`DELETE FROM mounts WHERE mountpoint = ?`, mountpoint)
	return err
}

// Lookup returns the entry for a mountpoint if it exists and its PID is still
// alive. Stale entries (dead PID) are automatically removed and (MountEntry{},
// false, nil) is returned.
func (r *MountRegistry) Lookup(mountpoint string) (MountEntry, bool, error) {
	row := r.db.QueryRow(
		`SELECT mountpoint, source, remote_name, sock_path, pid, mounted_at
		 FROM mounts WHERE mountpoint = ?`, mountpoint)
	var e MountEntry
	if err := row.Scan(&e.Mountpoint, &e.Source, &e.RemoteName, &e.SockPath, &e.PID, &e.MountedAt); err != nil {
		if err == sql.ErrNoRows {
			return MountEntry{}, false, nil
		}
		return MountEntry{}, false, err
	}
	if !pidAlive(e.PID) {
		_ = r.Deregister(mountpoint)
		return MountEntry{}, false, nil
	}
	return e, true, nil
}

// ListBySource returns all live entries whose Source matches the given string.
func (r *MountRegistry) ListBySource(source string) ([]MountEntry, error) {
	return r.queryEntries(`SELECT mountpoint, source, remote_name, sock_path, pid, mounted_at
		FROM mounts WHERE source = ?`, source)
}

// ListAll returns all live registry entries, lazily purging stale ones.
func (r *MountRegistry) ListAll() ([]MountEntry, error) {
	return r.queryEntries(`SELECT mountpoint, source, remote_name, sock_path, pid, mounted_at
		FROM mounts`)
}

func (r *MountRegistry) queryEntries(query string, args ...interface{}) ([]MountEntry, error) {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MountEntry
	var stale []string
	for rows.Next() {
		var e MountEntry
		if err := rows.Scan(&e.Mountpoint, &e.Source, &e.RemoteName, &e.SockPath, &e.PID, &e.MountedAt); err != nil {
			return nil, err
		}
		if !pidAlive(e.PID) {
			stale = append(stale, e.Mountpoint)
			continue
		}
		results = append(results, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, mp := range stale {
		_ = r.Deregister(mp)
	}
	return results, nil
}

// pidAlive reports whether a process with the given PID is still running.
// It uses signal 0, which checks for process existence without sending a signal.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
