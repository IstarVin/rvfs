// Package ipc defines the UNIX-socket protocol used between a running
// rvfs mount process and CLI subcommands (status, sync, etc.).
//
// Each message is a newline-terminated JSON object. The client sends a
// Request and the server replies with a Response whose Type matches.
package ipc

// Request is sent by the client to the server.
type Request struct {
	// Cmd is the command name: "status" or "sync".
	Cmd string `json:"cmd"`
	// Force is used with Cmd=="sync" to clear all retry_after timers first.
	Force bool `json:"force,omitempty"`
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

// SockPath returns the UNIX socket path for a given remoteID.
// The socket is placed in XDG_RUNTIME_DIR or ~/.local/share/rvfs/.
func SockPath(remoteID string) string {
	return sockPath(remoteID)
}
