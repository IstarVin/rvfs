package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/fuse"
	"github.com/IstarVin/rvfs/internal/remote/gdrive"
	syncpkg "github.com/IstarVin/rvfs/internal/sync"
	"github.com/spf13/cobra"
)

var (
	mountDebug            bool
	mountCacheDir         string
	mountPollInterval     time.Duration
	mountProbeInterval    time.Duration
	mountRecoveryInterval time.Duration
)

var mountCmd = &cobra.Command{
	Use:   "mount <source> <mountpoint>",
	Short: "Mount a local directory or remote via FUSE",
	Long: `Mount a filesystem via FUSE.

For local backing directory:
  rvfs mount /path/to/dir /mnt/point

For a configured remote:
  rvfs mount gdrive:Documents /mnt/point
  rvfs mount myremote: /mnt/point   (mount root)`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source := args[0]
		mountpoint := args[1]

		cacheDir := mountCacheDir
		if cacheDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("home dir: %w", err)
			}
			cacheDir = filepath.Join(home, ".cache", "rvfs")
		}

		// Determine if source is a remote (contains ':') or a local path.
		if before, after, ok := strings.Cut(source, ":"); ok {
			return mountRemote(before, after, mountpoint, cacheDir)
		}
		return mountLocal(source, mountpoint, cacheDir)
	},
}

func mountLocal(backingDir, mountpoint, cacheDir string) error {
	remoteID := filepath.Base(backingDir)

	cl, server, err := fuse.Mount(cacheDir, remoteID, mountpoint, fuse.MountOptions{
		Debug: mountDebug,
	})
	if err != nil {
		return fmt.Errorf("mount: %w", err)
	}

	if err := cl.SeedFromDir(backingDir); err != nil {
		server.Unmount()
		cl.Close()
		return fmt.Errorf("seed cache: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Mounted %s at %s (press Ctrl-C to unmount)\n", backingDir, mountpoint)
	server.Wait()
	cl.Close()
	return nil
}

func mountRemote(remoteName, remotePath, mountpoint, cacheDir string) error {
	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	rc, exists := cfg.Remotes[remoteName]
	if !exists {
		return fmt.Errorf("remote %q not found. Run 'rvfs remote add' first", remoteName)
	}

	if rc.Type != "gdrive" {
		return fmt.Errorf("unsupported remote type %q", rc.Type)
	}

	// Merge the remote's configured root path with the mount sub-path.
	rootPath := rc.RootPath
	if remotePath != "" {
		if rootPath == "" {
			rootPath = remotePath
		} else {
			rootPath = rootPath + "/" + remotePath
		}
	}

	remoteID := remoteName
	tokenPath := config.TokenPath(remoteName)

	// Create cache layer first (needed by the adapter for path→ID cache).
	cl, err := cache.NewCacheLayer(cacheDir, remoteID)
	if err != nil {
		return fmt.Errorf("cache layer: %w", err)
	}

	adapter, err := gdrive.New(rc.ClientID, rc.ClientSecret, tokenPath, rootPath, cl.DB())
	if err != nil {
		cl.Close()
		return fmt.Errorf("create gdrive adapter: %w", err)
	}

	// Probe connectivity. If the probe fails but a local cache DB already
	// exists, allow mounting offline so cached files remain accessible.
	if probeErr := adapter.Probe(); probeErr != nil {
		dbPath := filepath.Join(cacheDir, remoteID, "meta.db")
		if _, statErr := os.Stat(dbPath); statErr != nil {
			// No local cache — nothing useful to serve offline.
			cl.Close()
			return fmt.Errorf("probe remote: %w", probeErr)
		}
		fmt.Fprintf(os.Stderr, "Warning: remote unreachable (%v); mounting offline from cache\n", probeErr)
	}

	// Start the connectivity monitor.
	mon := connectivity.New(adapter, mountProbeInterval, 3)
	mon.SetRecoveryInterval(mountRecoveryInterval)
	mon.Start()
	defer mon.Stop()

	_, server, err := fuse.Mount(cacheDir, remoteID, mountpoint, fuse.MountOptions{
		Debug:   mountDebug,
		Adapter: adapter,
		Monitor: mon,
	})
	if err != nil {
		cl.Close()
		return fmt.Errorf("mount: %w", err)
	}

	// Start sync engine.
	engine := syncpkg.NewEngine(adapter, cl, mountPollInterval, mon)
	engine.Start()

	// Do initial pull to populate the root directory listing.
	if err := engine.PullOnce(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: initial pull failed: %v\n", err)
	}

	label := remoteName + ":" + remotePath
	fmt.Fprintf(os.Stderr, "Mounted %s at %s (press Ctrl-C to unmount)\n", label, mountpoint)
	server.Wait()
	engine.Stop()
	cl.Close()
	return nil
}

func init() {
	mountCmd.Flags().BoolVar(&mountDebug, "debug", false, "Enable FUSE debug logging")
	mountCmd.Flags().StringVar(&mountCacheDir, "cache-dir", "", "Cache directory (default ~/.cache/rvfs)")
	mountCmd.Flags().DurationVar(&mountPollInterval, "poll-interval", 30*time.Second, "Remote polling interval")
	mountCmd.Flags().DurationVar(&mountProbeInterval, "probe-interval", 5*time.Second, "Connectivity probe interval")
	mountCmd.Flags().DurationVar(&mountRecoveryInterval, "recovery-interval", 2*time.Second, "Probe interval while offline (for faster reconnect detection)")
}
