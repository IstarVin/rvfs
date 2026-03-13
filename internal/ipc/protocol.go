// Package ipc defines the UNIX-socket protocol used between a running
// rvfs mount process and CLI subcommands (status, sync, etc.).
//
// Each message is a newline-terminated JSON object. The client sends a
// Request and the server replies with a Response whose Type matches.
package ipc

// Request is sent by the client to the server.
type Request struct {
	// Cmd is the command name: "status", "sync", "prefetch", "evict", or "downloads".
	Cmd string `json:"cmd"`
	// Force is used with Cmd=="sync" to clear all retry_after timers first.
	Force bool `json:"force,omitempty"`
	// Path is used with Cmd=="prefetch", "evict", or "downloads".
	// For "downloads", an empty path means "list active downloads".
	Path string `json:"path,omitempty"`
}

// StatusResponse is the server's reply to a "status" request.
type StatusResponse struct {
	// Source is the remote source string used to mount (e.g. "gdrive:Documents").
	Source string `json:"source"`
	// Mountpoint is the FUSE mountpoint path.
	Mountpoint string `json:"mountpoint"`
	// Online is "true" when the remote is reachable, "false" otherwise.
	Online bool `json:"online"`
	// CacheUsed is the total on-disk bytes currently used in the files/ dir.
	CacheUsed int64 `json:"cache_used"`
	// CacheTotal is the configured max cache size (0 = unlimited).
	CacheTotal int64 `json:"cache_total"`
	// CacheMinFreeSpace is the configured minimum free space threshold (0 = disabled).
	CacheMinFreeSpace int64 `json:"cache_min_free_space,omitempty"`
	// CacheFSFree is the current free bytes on the filesystem containing the cache dir.
	CacheFSFree int64 `json:"cache_fs_free,omitempty"`
	// Pending is the number of rows in the pending_ops table.
	Pending int `json:"pending"`
	// Conflicts is the number of rows in the conflicts table.
	Conflicts int `json:"conflicts"`
}

// SyncResponse is the server's reply to a "sync" request.
type SyncResponse struct {
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// ActionResponse is the server's reply to a command that mutates state.
type ActionResponse struct {
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// DownloadStatusEntry describes one path's download/cache status.
type DownloadStatusEntry struct {
	Path       string `json:"path"`
	State      string `json:"state"`
	Downloaded int64  `json:"downloaded"`
	TotalSize  int64  `json:"total_size"`
	Done       bool   `json:"done"`
	Err        string `json:"err,omitempty"`
}

// DownloadStatusResponse is the server's reply to a "downloads" request.
type DownloadStatusResponse struct {
	Entries []DownloadStatusEntry `json:"entries"`
}

// SockPath returns the UNIX socket path for a given remoteID.
// The socket is placed in XDG_RUNTIME_DIR or ~/.local/share/rvfs/.
func SockPath(remoteID string) string {
	return sockPath(remoteID)
}
