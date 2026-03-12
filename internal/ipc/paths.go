package ipc

import (
	"os"
	"path/filepath"
)

// sockPath returns the UNIX socket path for a remoteID.
// It uses XDG_RUNTIME_DIR when available, otherwise ~/.local/share/rvfs/.
func sockPath(remoteID string) string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/tmp"
		}
		dir = filepath.Join(home, ".local", "share", "rvfs")
	} else {
		dir = filepath.Join(dir, "rvfs")
	}
	return filepath.Join(dir, remoteID+".sock")
}

// ensureSockDir creates the directory that will hold the socket file.
func ensureSockDir(sockfile string) error {
	return os.MkdirAll(filepath.Dir(sockfile), 0700)
}
