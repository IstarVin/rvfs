package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/IstarVin/rvfs/internal/fuse"
	"github.com/spf13/cobra"
)

var (
	mountDebug    bool
	mountCacheDir string
)

var mountCmd = &cobra.Command{
	Use:   "mount <backing-dir> <mountpoint>",
	Short: "Mount a local directory via FUSE",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		backingDir := args[0]
		mountpoint := args[1]

		cacheDir := mountCacheDir
		if cacheDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("home dir: %w", err)
			}
			cacheDir = filepath.Join(home, ".cache", "rvfs")
		}

		// Use the backing dir's base name as the remote-id for now.
		remoteID := filepath.Base(backingDir)

		cl, server, err := fuse.Mount(cacheDir, remoteID, mountpoint, fuse.MountOptions{
			Debug: mountDebug,
		})
		if err != nil {
			return fmt.Errorf("mount: %w", err)
		}

		// Seed cache from the backing directory.
		if err := cl.SeedFromDir(backingDir); err != nil {
			server.Unmount()
			cl.Close()
			return fmt.Errorf("seed cache: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Mounted %s at %s (press Ctrl-C to unmount)\n", backingDir, mountpoint)
		server.Wait()
		cl.Close()
		return nil
	},
}

func init() {
	mountCmd.Flags().BoolVar(&mountDebug, "debug", false, "Enable FUSE debug logging")
	mountCmd.Flags().StringVar(&mountCacheDir, "cache-dir", "", "Cache directory (default ~/.cache/rvfs)")
}
