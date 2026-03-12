package remote

import (
	"context"
	"io"
	"time"
)

// FileInfo describes a remote file or directory.
type FileInfo struct {
	Path     string
	Name     string
	Size     int64
	IsDir    bool
	Mtime    time.Time
	Checksum string
}

// RemoteAdapter is implemented by every remote storage backend (Google Drive,
// SFTP, S3, …). All paths are relative to the configured remote root.
type RemoteAdapter interface {
	// List returns the immediate children of path.
	// Use "" for the root directory.
	List(path string) ([]FileInfo, error)

	// Stat returns metadata for a single remote path.
	Stat(path string) (FileInfo, error)

	// Get downloads the entire file and writes it to dest.
	Get(path string, dest io.Writer) error

	// GetRange downloads a byte range [offset, offset+length) and writes it
	// to dest. Backends that do not support range requests should return
	// ErrRangeNotSupported.
	GetRange(path string, offset, length int64, dest io.Writer) error

	// Put uploads the contents of src as a file at path.
	// The caller may cancel ctx to abort the upload.
	Put(ctx context.Context, path string, src io.Reader, size int64, mtime time.Time) error

	// Delete removes a file at path.
	Delete(path string) error

	// Mkdir creates a directory at path.
	Mkdir(path string) error

	// Rename moves src to dst.
	Rename(src, dst string) error

	// Probe performs a lightweight connectivity check and returns nil on
	// success.
	Probe() error

	// SupportsRange reports whether the backend supports GetRange.
	SupportsRange() bool
}
