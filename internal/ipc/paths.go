package ipc

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
)

// sockDir returns the directory used for socket files.
// It prefers XDG_RUNTIME_DIR; otherwise ~/.local/share/rvfs.
func sockDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/tmp"
		}
		return filepath.Join(home, ".local", "share", "rvfs")
	}
	return filepath.Join(dir, "rvfs")
}

// SockDir returns the directory that holds socket and registry files.
func SockDir() string { return sockDir() }

// sockPath returns the UNIX socket path for a remoteID (legacy single-mount path).
func sockPath(remoteID string) string {
	return filepath.Join(sockDir(), remoteID+".sock")
}

// MountSockPath returns a unique socket path for a (remoteName, mountpoint)
// pair, allowing multiple simultaneous mounts of the same remote.
// mountpoint must be an absolute path.
func MountSockPath(remoteName, mountpoint string) string {
	h := fnv.New32a()
	fmt.Fprint(h, mountpoint)
	return filepath.Join(sockDir(), fmt.Sprintf("%s_%08x.sock", remoteName, h.Sum32()))
}

// MountRegPath returns the path to the mount-registry SQLite database.
func MountRegPath() string {
	return filepath.Join(sockDir(), "mounts.db")
}

// ensureSockDir creates the directory that will hold the socket file.
func ensureSockDir(sockfile string) error {
	return os.MkdirAll(filepath.Dir(sockfile), 0700)
}
