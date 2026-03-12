package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/ipc"
)

// resolveSource parses a source argument (e.g. "gdrive:Documents" or
// "myremote:") and returns the remoteID, the ipc socket path and an open
// CacheLayer. The caller must Close the CacheLayer when done.
//
// The socket path is resolved via the mount registry when possible. If the
// registry shows multiple active mounts for the same source an error is
// returned; callers should direct the user to `rvfs status` for disambiguation.
func resolveSource(source string) (remoteID string, sockPath string, cl *cache.CacheLayer, err error) {
	before, _, ok := strings.Cut(source, ":")
	if !ok {
		err = fmt.Errorf("invalid source %q: expected <remote>:<path>", source)
		return
	}
	remoteID = before

	dir := getCacheDir()
	cl, err = cache.NewCacheLayer(dir, remoteID)
	if err != nil {
		err = fmt.Errorf("open cache for %q: %w", remoteID, err)
		return
	}

	// Try to get the exact socket path from the mount registry.
	reg, regErr := ipc.OpenMountRegistry()
	if regErr == nil {
		defer reg.Close()
		entries, listErr := reg.ListBySource(source)
		if listErr == nil {
			switch len(entries) {
			case 0:
				// Mount not running; leave sockPath empty so callers handle it.
			case 1:
				sockPath = entries[0].SockPath
				return
			default:
				err = fmt.Errorf(
					"multiple active mounts for %q; use 'rvfs status' to list them",
					source,
				)
				cl.Close()
				return
			}
		}
	}

	// Fallback: use the legacy per-remote socket path.
	sockPath = ipc.SockPath(remoteID)
	return
}

// getCacheDir returns the effective cache directory, honouring the global
// config when available.
func getCacheDir() string {
	if globalCfg != nil && globalCfg.Mount.CacheDir != "" {
		return globalCfg.Mount.CacheDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", "rvfs")
	}
	return filepath.Join(home, ".cache", "rvfs")
}
