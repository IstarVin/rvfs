package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/fuse"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/IstarVin/rvfs/internal/remote"
	"github.com/IstarVin/rvfs/internal/remote/gdrive"
	"github.com/IstarVin/rvfs/internal/service"
	syncpkg "github.com/IstarVin/rvfs/internal/sync"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	mountDebug             bool
	mountForeground        bool
	mountLogLevel          string
	mountVerifyChecksums   bool
	mountInstallService    bool
	mountUninstallService  bool
	mountCacheDir          string
	mountPollInterval      time.Duration
	mountProbeInterval     time.Duration
	mountRecoveryInterval  time.Duration
	mountReadAhead         int64
	mountIdleTimeout       time.Duration
	mountConflictStrategy  string
	mountCacheSize         int64
	mountCacheMaxAge       time.Duration
	mountCacheMinFreeSpace int64
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

		// Handle --install-service / --uninstall-service before anything else.
		if mountInstallService || mountUninstallService {
			return handleServiceInstall(source, mountpoint, args, cmd)
		}

		// Apply config-file defaults for any flag the user did not explicitly set.
		if globalCfg != nil {
			mc := globalCfg.Mount
			if !cmd.Flags().Changed("debug") {
				mountDebug = mc.Debug
			}
			if !cmd.Flags().Changed("cache-dir") {
				mountCacheDir = mc.CacheDir
			}
			if !cmd.Flags().Changed("poll-interval") {
				mountPollInterval = mc.PollInterval.D()
			}
			if !cmd.Flags().Changed("probe-interval") {
				mountProbeInterval = mc.ProbeInterval.D()
			}
			if !cmd.Flags().Changed("recovery-interval") {
				mountRecoveryInterval = mc.RecoveryInterval.D()
			}
			if !cmd.Flags().Changed("read-ahead") {
				mountReadAhead = mc.ReadAhead.Int64()
			}
			if !cmd.Flags().Changed("idle-timeout") {
				mountIdleTimeout = mc.IdleTimeout.D()
			}
			if !cmd.Flags().Changed("conflict") {
				mountConflictStrategy = mc.ConflictStrategy
			}
			if !cmd.Flags().Changed("cache-size") {
				mountCacheSize = mc.CacheSize.Int64()
			}
			if !cmd.Flags().Changed("cache-max-age") {
				mountCacheMaxAge = mc.CacheMaxAge.D()
			}
			if !cmd.Flags().Changed("cache-min-free-space") {
				mountCacheMinFreeSpace = mc.CacheMinFreeSpace.Int64()
			}
		}

		cacheDir := mountCacheDir
		if cacheDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("home dir: %w", err)
			}
			cacheDir = filepath.Join(home, ".cache", "rvfs")
		}

		if !syncpkg.ValidConflictStrategy(mountConflictStrategy) {
			return fmt.Errorf("invalid --conflict %q: must be one of both, local-wins, remote-wins, manual", mountConflictStrategy)
		}

		// Configure structured logging.
		var logLevel slog.Level
		switch strings.ToLower(mountLogLevel) {
		case "debug":
			logLevel = slog.LevelDebug
		case "warn", "warning":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		default:
			logLevel = slog.LevelInfo
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

		// Check for duplicate mountpoint before daemonizing (fail fast with visible error).
		absMountpoint, err := filepath.Abs(mountpoint)
		if err != nil {
			return fmt.Errorf("resolve mountpoint: %w", err)
		}
		reg, regErr := ipc.OpenMountRegistry()
		if regErr == nil && reg != nil {
			defer reg.Close()
			if entry, alive, _ := reg.Lookup(absMountpoint); alive {
				return fmt.Errorf("mountpoint %q is already in use (source: %s, pid: %d)",
					absMountpoint, entry.Source, entry.PID)
			}
		}

		// Daemonize by re-launching with --foreground unless already in foreground.
		if !mountForeground {
			newArgs := append(os.Args[1:], "--foreground")
			cmd := exec.Command(os.Args[0], newArgs...)
			cmd.Stdin = nil
			cmd.Stdout = nil
			cmd.Stderr = nil
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("daemonize: %w", err)
			}
			fmt.Fprintf(os.Stdout, "rvfs daemon started (pid %d)\n", cmd.Process.Pid)
			return nil
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

	slog.Info("mounted", "source", backingDir, "mountpoint", mountpoint)
	server.Wait()
	cl.Close()
	return nil
}

func mountRemote(remoteName, remotePath, mountpoint, cacheDir string) error {
	absMountpoint, err := filepath.Abs(mountpoint)
	if err != nil {
		return fmt.Errorf("resolve mountpoint: %w", err)
	}

	// Check for duplicate (same remote → same mountpoint) via the mount registry.
	reg, regErr := ipc.OpenMountRegistry()
	if regErr != nil {
		slog.Warn("mount registry unavailable", "err", regErr)
		reg = nil
	}
	if reg != nil {
		defer reg.Close()
		if entry, alive, _ := reg.Lookup(absMountpoint); alive {
			return fmt.Errorf("mountpoint %q is already in use (source: %s, pid: %d)",
				absMountpoint, entry.Source, entry.PID)
		}
	}

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
		slog.Warn("remote unreachable; mounting offline from cache", "err", probeErr)
	}

	// Recover any downloads that were interrupted by a previous crash.
	recoverDownloads(cl, adapter)

	// Start the connectivity monitor.
	mon := connectivity.New(adapter, mountProbeInterval, 3)
	mon.SetRecoveryInterval(mountRecoveryInterval)
	mon.Start()
	defer mon.Stop()

	// Create the sync engine before mounting so we can pass it to the FUSE
	// layer for upload cancellation on Unlink. Start it after the mount is up.
	engine := syncpkg.NewEngine(adapter, cl, mountPollInterval, mon, syncpkg.ConflictStrategy(mountConflictStrategy))

	_, server, err := fuse.Mount(cacheDir, remoteID, mountpoint, fuse.MountOptions{
		Debug:           mountDebug,
		Adapter:         adapter,
		Monitor:         mon,
		ReadAhead:       mountReadAhead,
		IdleTimeout:     mountIdleTimeout,
		VerifyChecksums: mountVerifyChecksums,
		SyncEngine:      engine,
	})
	if err != nil {
		cl.Close()
		return fmt.Errorf("mount: %w", err)
	}

	// Start sync engine.
	engine.Start()

	// Start IPC server so status/sync commands can communicate with us.
	source := remoteName + ":" + remotePath
	sockPath := ipc.MountSockPath(remoteName, absMountpoint)
	srv := ipc.NewServer(sockPath, &mountHandler{
		source:     source,
		mountpoint: absMountpoint,
		cl:         cl,
		engine:     engine,
		mon:        mon,
		maxSize:    mountCacheSize,
	})
	if listenErr := srv.Listen(); listenErr != nil {
		slog.Warn("IPC server unavailable", "err", listenErr)
	} else {
		defer srv.Close()
		if reg != nil {
			if regErr := reg.Register(ipc.MountEntry{
				Mountpoint: absMountpoint,
				Source:     source,
				RemoteName: remoteName,
				SockPath:   sockPath,
				PID:        os.Getpid(),
				MountedAt:  time.Now().Unix(),
			}); regErr != nil {
				slog.Warn("mount registry: register failed", "err", regErr)
			} else {
				defer reg.Deregister(absMountpoint)
			}
		}
	}

	// Start LRU evictor.
	evCtx, evCancel := context.WithCancel(context.Background())
	defer evCancel()
	ev := &cache.Evictor{MaxSize: mountCacheSize, MaxAge: mountCacheMaxAge, MinFreeSpace: mountCacheMinFreeSpace}
	go ev.Run(evCtx, cl)

	// Do an initial pull only when the local metadata DB is empty.
	// If data is already present, the background sync engine will handle
	// any remote changes without blocking the mount.
	if hasData, err := cl.DB().HasFiles(); err != nil {
		slog.Warn("checking cache state failed", "err", err)
	} else if !hasData {
		if err := engine.PullOnce(); err != nil {
			slog.Warn("initial pull failed", "err", err)
		}
	}

	label := remoteName + ":" + remotePath
	slog.Info("mounted", "source", label, "mountpoint", mountpoint)
	server.Wait()
	engine.Stop()
	cl.Close()
	return nil
}

// recoverDownloads inspects the DB for files left in StateDownloading (which
// means a previous process was killed mid-download) and either resets them to
// StateEvicted (for adapters without range support) or leaves them as
// StateDownloading so that Manager.Start() can resume from the persisted
// CachedRanges on the next Open call.
func recoverDownloads(cl *cache.CacheLayer, adapter remote.RemoteAdapter) {
	entries, err := cl.DB().ListByState(cache.StateDownloading)
	if err != nil || len(entries) == 0 {
		return
	}
	for _, e := range entries {
		if !adapter.SupportsRange() {
			// Cannot resume from partial ranges — restart from scratch.
			if setErr := cl.DB().SetState(e.Path, cache.StateEvicted); setErr != nil {
				slog.Warn("recover download: reset to evicted", "path", e.Path, "err", setErr)
			} else {
				slog.Info("recover download: reset to evicted (no range support)", "path", e.Path)
			}
		} else {
			// Leave as StateDownloading; Manager.Start() will reload CachedRanges
			// and resume from gaps on the next Open.
			slog.Info("recover download: resumable via persisted ranges", "path", e.Path)
		}
	}
}

// mountHandler implements ipc.Handler for a running mount process.
type mountHandler struct {
	source     string
	mountpoint string
	cl         *cache.CacheLayer
	engine     *syncpkg.Engine
	mon        *connectivity.Monitor
	maxSize    int64
}

// handleServiceInstall handles --install-service and --uninstall-service. It
// detects the host OS at runtime and delegates to the appropriate backend.
func handleServiceInstall(source, mountpoint string, _ []string, cmd *cobra.Command) error {
	name := strings.NewReplacer(":", "-", "/", "-").Replace(source)
	if name == "" {
		name = "default"
	}

	// Build the extra flags to preserve (skip service-control flags).
	skip := map[string]bool{
		"install-service": true, "uninstall-service": true,
		"foreground": true, // added by the service template
	}
	var extra []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if !skip[f.Name] {
			extra = append(extra, "--"+f.Name+"="+f.Value.String())
		}
	})

	if mountUninstallService {
		// Try systemd first, then launchd.
		if err := service.UninstallSystemdService(name); err != nil {
			return service.UninstallLaunchdService(name)
		}
		return nil
	}

	// --install-service: pick backend based on OS.
	if _, err := exec.LookPath("systemctl"); err == nil {
		return service.InstallSystemdService(name, source, mountpoint, extra)
	}
	if _, err := exec.LookPath("launchctl"); err == nil {
		return service.InstallLaunchdService(name, source, mountpoint, extra)
	}
	return fmt.Errorf("--install-service: neither systemctl nor launchctl found on PATH")
}

func (h *mountHandler) HandleStatus() (ipc.StatusResponse, error) {
	pending, _ := h.cl.DB().CountPendingOps()
	conflicts, _ := h.cl.DB().CountConflicts()
	used, _ := cache.DirSize(h.cl.FilesDir())
	online := h.mon != nil && h.mon.State() == connectivity.StateOnline
	return ipc.StatusResponse{
		Source:     h.source,
		Mountpoint: h.mountpoint,
		Online:     online,
		CacheUsed:  used,
		CacheTotal: h.maxSize,
		Pending:    pending,
		Conflicts:  conflicts,
	}, nil
}

func (h *mountHandler) HandleSync(force bool) error {
	if force {
		if err := h.cl.DB().ResetRetryAfter(); err != nil {
			return err
		}
	}
	go func() {
		_ = h.engine.PullOnce()
	}()
	return nil
}

// durationValue is a pflag.Value that parses Go duration strings
// case-insensitively, so "3H" is treated the same as "3h".
type durationValue time.Duration

func (d *durationValue) Set(s string) error {
	v, err := time.ParseDuration(strings.ToLower(s))
	if err != nil {
		return err
	}
	*d = durationValue(v)
	return nil
}
func (d *durationValue) Type() string   { return "duration" }
func (d *durationValue) String() string { return time.Duration(*d).String() }

// byteSizeValue is a pflag.Value that parses byte sizes with optional
// case-insensitive suffixes: K (kibibytes), M (mebibytes), G (gibibytes).
// Examples: "256", "4K", "3M", "1G".
type byteSizeValue int64

func (b *byteSizeValue) Set(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("empty byte size")
	}
	multiplier := int64(1)
	numStr := s
	switch strings.ToUpper(string(s[len(s)-1])) {
	case "K":
		multiplier = 1024
		numStr = s[:len(s)-1]
	case "M":
		multiplier = 1024 * 1024
		numStr = s[:len(s)-1]
	case "G":
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid byte size %q", s)
	}
	*b = byteSizeValue(n * multiplier)
	return nil
}
func (b *byteSizeValue) Type() string   { return "bytes" }
func (b *byteSizeValue) String() string { return strconv.FormatInt(int64(*b), 10) }

func init() {
	mountCmd.Flags().BoolVar(&mountDebug, "debug", false, "Enable FUSE debug logging")
	mountCmd.Flags().BoolVar(&mountForeground, "foreground", false, "Run in foreground instead of daemonizing")
	mountCmd.Flags().StringVar(&mountLogLevel, "log-level", "info", "Log level: debug, info, warn, error")
	mountCmd.Flags().BoolVar(&mountVerifyChecksums, "verify-checksums", false, "Verify SHA256 checksum of cached files on open (off by default for performance)")
	mountCmd.Flags().StringVar(&mountCacheDir, "cache-dir", "", "Cache directory (default ~/.cache/rvfs)")

	mountPollInterval = 30 * time.Second
	mountCmd.Flags().Var((*durationValue)(&mountPollInterval), "poll-interval", "Remote polling interval (e.g. 30s, 5M, 1H)")

	mountProbeInterval = 5 * time.Second
	mountCmd.Flags().Var((*durationValue)(&mountProbeInterval), "probe-interval", "Connectivity probe interval (e.g. 5s, 1M)")

	mountRecoveryInterval = 2 * time.Second
	mountCmd.Flags().Var((*durationValue)(&mountRecoveryInterval), "recovery-interval", "Probe interval while offline for faster reconnect detection (e.g. 2s)")

	mountReadAhead = 64 * 1024 * 1024
	mountCmd.Flags().Var((*byteSizeValue)(&mountReadAhead), "read-ahead", "Bytes to download ahead of the read position; supports K/M/G suffixes (e.g. 256, 4K, 3M); 0 = unlimited")

	mountIdleTimeout = 5
	mountCmd.Flags().Var((*durationValue)(&mountIdleTimeout), "idle-timeout", "Stop downloading when paused with no reads for this duration (e.g. 30s, 5M); 0 = wait forever; requires --read-ahead > 0")

	mountConflictStrategy = "both"
	mountCmd.Flags().StringVar(&mountConflictStrategy, "conflict", "both", "Conflict resolution strategy: both, local-wins, remote-wins, manual")

	mountCacheSize = 20 * 1024 * 1024 * 1024 // 20 GiB default
	mountCmd.Flags().Var((*byteSizeValue)(&mountCacheSize), "cache-size", "Maximum total cache size; evict clean files when exceeded (e.g. 10G, 500M); 0 = unlimited")

	mountCmd.Flags().Var((*durationValue)(&mountCacheMaxAge), "cache-max-age", "Evict clean files not accessed for this long (e.g. 7d=168h, 30d=720h); 0 = disabled")

	mountCmd.Flags().Var((*byteSizeValue)(&mountCacheMinFreeSpace), "cache-min-free-space", "Minimum free space to maintain on the cache filesystem; evict clean files when below this threshold (e.g. 1G, 500M); 0 = disabled")

	mountCmd.Flags().BoolVar(&mountInstallService, "install-service", false, "Install OS service (systemd/launchd) to auto-start on login and exit")
	mountCmd.Flags().BoolVar(&mountUninstallService, "uninstall-service", false, "Remove previously installed OS service and exit")
}
