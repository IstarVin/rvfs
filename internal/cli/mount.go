package cli

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/connectivity"
	"github.com/IstarVin/rvfs/internal/download"
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
	mountDaemonFd          int
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
			statusR, statusW, pipeErr := os.Pipe()
			if pipeErr != nil {
				return fmt.Errorf("daemonize: create startup pipe: %w", pipeErr)
			}
			newArgs := append(os.Args[1:], "--foreground", "--daemon-fd=3")
			daemonCmd := exec.Command(os.Args[0], newArgs...)
			daemonCmd.Stdin = nil
			daemonCmd.Stdout = nil
			daemonCmd.Stderr = nil
			daemonCmd.ExtraFiles = []*os.File{statusW} // becomes fd 3 in child
			daemonCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := daemonCmd.Start(); err != nil {
				statusR.Close()
				statusW.Close()
				return fmt.Errorf("daemonize: %w", err)
			}
			statusW.Close() // parent only reads
			fatalMsg, warnings := readDaemonStartup(statusR)
			statusR.Close()
			for _, w := range warnings {
				printWarning(cmd.ErrOrStderr(), "%s", w)
			}
			if fatalMsg != "" {
				_ = daemonCmd.Process.Kill()
				return fmt.Errorf("%s", fatalMsg)
			}
			printSection(cmd.OutOrStdout(), "Mount ready")
			printKeyValues(cmd.OutOrStdout(), [][2]string{{"Source:", source}, {"Mount:", absMountpoint}, {"PID:", fmt.Sprintf("%d", daemonCmd.Process.Pid)}})
			printHint(cmd.OutOrStdout(), "run 'rvfs status %s' to inspect mount health", source)
			fprintln(cmd.OutOrStdout())
			return nil
		}

		// Set up startup reporter when spawned as a daemon child (--daemon-fd set by parent).
		sr := newStartupReporter(mountDaemonFd)
		defer sr.close()

		// In daemon-child mode, redirect slog to a per-remote log file so
		// post-detach diagnostics are not silently discarded.
		if sr != nil {
			var logRemoteID string
			if before, _, ok := strings.Cut(source, ":"); ok {
				logRemoteID = before
			} else {
				logRemoteID = filepath.Base(source)
			}
			logDir := filepath.Join(cacheDir, logRemoteID)
			if err := os.MkdirAll(logDir, 0755); err == nil {
				logPath := filepath.Join(logDir, "daemon.log")
				if lf, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
					slog.SetDefault(slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: logLevel})))
				} else {
					sr.warn(fmt.Sprintf("could not open daemon log: %v", err))
				}
			}
		}

		// Determine if source is a remote (contains ':') or a local path.
		var mountErr error
		if before, after, ok := strings.Cut(source, ":"); ok {
			mountErr = mountRemote(before, after, mountpoint, cacheDir, sr)
		} else {
			mountErr = mountLocal(source, mountpoint, cacheDir, sr)
		}
		if mountErr != nil {
			sr.fatal(mountErr.Error())
			return mountErr
		}
		return nil
	},
}

// startupReporter sends structured startup events to the parent daemon-watcher
// via an inherited file descriptor. A nil receiver is always safe (all methods
// are no-ops), which keeps --foreground-without-parent usage unchanged.
type startupReporter struct {
	w      *bufio.Writer
	f      *os.File
	closed bool
}

// newStartupReporter returns a reporter writing to fd, or nil if fd < 0.
func newStartupReporter(fd int) *startupReporter {
	if fd < 0 {
		return nil
	}
	f := os.NewFile(uintptr(fd), "startup-report")
	return &startupReporter{w: bufio.NewWriter(f), f: f}
}

func (r *startupReporter) warn(msg string) {
	if r == nil || r.closed {
		return
	}
	fmt.Fprintf(r.w, "WARN:%s\n", strings.ReplaceAll(msg, "\n", " "))
	r.w.Flush()
}

func (r *startupReporter) fatal(msg string) {
	if r == nil || r.closed {
		return
	}
	r.closed = true
	fmt.Fprintf(r.w, "FATAL:%s\n", strings.ReplaceAll(msg, "\n", " "))
	r.w.Flush()
	r.f.Close()
}

func (r *startupReporter) ready() {
	if r == nil || r.closed {
		return
	}
	r.closed = true
	fmt.Fprintf(r.w, "READY\n")
	r.w.Flush()
	r.f.Close()
}

// close is a safety-net finaliser called via defer in RunE. It closes the pipe
// without sending any event, producing EOF on the parent's read end which it
// treats as an unexpected exit. It is a no-op when ready or fatal already fired.
func (r *startupReporter) close() {
	if r == nil || r.closed {
		return
	}
	r.closed = true
	r.f.Close()
}

// readDaemonStartup reads startup events from the child's pipe until the child
// signals READY or FATAL, or the pipe closes unexpectedly (child crashed).
// Returns ("", warnings) on success or (fatalMsg, warnings) on failure.
func readDaemonStartup(r *os.File) (fatalMsg string, warnings []string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "READY":
			return "", warnings
		case strings.HasPrefix(line, "FATAL:"):
			fatalMsg = strings.TrimPrefix(line, "FATAL:")
		case strings.HasPrefix(line, "WARN:"):
			warnings = append(warnings, strings.TrimPrefix(line, "WARN:"))
		}
	}
	// EOF: child sent FATAL then closed the pipe, or exited unexpectedly.
	if fatalMsg == "" {
		fatalMsg = "daemon exited before signalling ready"
	}
	return fatalMsg, warnings
}

func mountLocal(backingDir, mountpoint, cacheDir string, sr *startupReporter) error {
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
	sr.ready()
	server.Wait()
	cl.Close()
	return nil
}

func mountRemote(remoteName, remotePath, mountpoint, cacheDir string, sr *startupReporter) error {
	absMountpoint, err := filepath.Abs(mountpoint)
	if err != nil {
		return fmt.Errorf("resolve mountpoint: %w", err)
	}

	// Check for duplicate (same remote → same mountpoint) via the mount registry.
	reg, regErr := ipc.OpenMountRegistry()
	if regErr != nil {
		slog.Warn("mount registry unavailable", "err", regErr)
		sr.warn(fmt.Sprintf("mount registry unavailable: %v", regErr))
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
	if probeErr := adapter.Probe(context.Background()); probeErr != nil {
		dbPath := filepath.Join(cacheDir, remoteID, "meta.db")
		if _, statErr := os.Stat(dbPath); statErr != nil {
			// No local cache — nothing useful to serve offline.
			cl.Close()
			return fmt.Errorf("probe remote: %w", probeErr)
		}
		slog.Warn("remote unreachable; mounting offline from cache", "err", probeErr)
		sr.warn(fmt.Sprintf("remote unreachable, mounting offline from cache: %v", probeErr))
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
	downloadMgr := download.NewManager(adapter, cl, mon, download.ManagerOptions{
		ReadAhead:   mountReadAhead,
		IdleTimeout: mountIdleTimeout,
	})

	_, server, err := fuse.Mount(cacheDir, remoteID, mountpoint, fuse.MountOptions{
		Debug:           mountDebug,
		Adapter:         adapter,
		Monitor:         mon,
		ReadAhead:       mountReadAhead,
		IdleTimeout:     mountIdleTimeout,
		VerifyChecksums: mountVerifyChecksums,
		SyncEngine:      engine,
		DownloadManager: downloadMgr,
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
	h := &mountHandler{
		source:       source,
		mountpoint:   absMountpoint,
		cl:           cl,
		engine:       engine,
		downloadMgr:  downloadMgr,
		mon:          mon,
		maxSize:      mountCacheSize,
		minFreeSpace: mountCacheMinFreeSpace,
		cacheDir:     cacheDir,
		prefetchQ:    make(chan prefetchRequest, 1024),
	}
	h.startPrefetchWorker()
	defer h.stopPrefetchWorker()

	srv := ipc.NewServer(sockPath, h)
	if listenErr := srv.Listen(); listenErr != nil {
		slog.Warn("IPC server unavailable", "err", listenErr)
		sr.warn(fmt.Sprintf("IPC server unavailable: %v", listenErr))
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
				sr.warn(fmt.Sprintf("mount registry: register failed: %v", regErr))
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

	// On first mount (no entries in the metadata DB), perform an initial pull
	// before signalling ready — this blocks the parent until the sync
	// completes so the mountpoint is fully populated when the command returns.
	if hasData, err := cl.DB().HasFiles(); err != nil {
		slog.Warn("checking cache state failed", "err", err)
	} else if !hasData {
		slog.Info("first mount: running initial sync before ready")
		if err := engine.PullOnce(); err != nil {
			slog.Warn("initial pull failed", "err", err)
		}
	}

	// All startup infrastructure is ready — signal the parent daemon-watcher
	// so it can detach. Post-startup logs continue to the daemon log file.
	label := remoteName + ":" + remotePath
	slog.Info("mounted", "source", label, "mountpoint", mountpoint)
	sr.ready()

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
			if setErr := cl.DB().MarkEvicted(e.Path); setErr != nil {
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
	source       string
	mountpoint   string
	cl           *cache.CacheLayer
	engine       *syncpkg.Engine
	downloadMgr  *download.Manager
	mon          *connectivity.Monitor
	maxSize      int64
	minFreeSpace int64
	cacheDir     string

	prefetchQ    chan prefetchRequest
	prefetchWG   sync.WaitGroup
	prefetchStop sync.Once
}

type prefetchRequest struct {
	path string
	size int64
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
	usage, _ := cache.DirUsage(h.cl.FilesDir())
	online := h.mon != nil && h.mon.State() == connectivity.StateOnline

	var fsFree int64
	if h.minFreeSpace > 0 && h.cacheDir != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(h.cacheDir, &st); err == nil {
			fsFree = int64(st.Bavail) * st.Bsize
		}
	}

	return ipc.StatusResponse{
		Source:            h.source,
		Mountpoint:        h.mountpoint,
		Online:            online,
		CacheUsed:         usage.PhysicalBytes,
		CacheLogicalUsed:  usage.LogicalBytes,
		CacheTotal:        h.maxSize,
		CacheMinFreeSpace: h.minFreeSpace,
		CacheFSFree:       fsFree,
		Pending:           pending,
		Conflicts:         conflicts,
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

func (h *mountHandler) HandlePrefetch(path string, sequential bool) error {
	if path == "" {
		return fmt.Errorf("prefetch: missing path")
	}
	if h.downloadMgr == nil {
		return fmt.Errorf("prefetch unavailable: mount has no remote adapter")
	}
	entry, err := h.cl.DB().GetFile(path)
	if err != nil {
		return err
	}
	if entry == nil {
		return fmt.Errorf("prefetch: path %q not found", path)
	}
	if entry.IsDir {
		return fmt.Errorf("prefetch: %q is a directory", path)
	}
	if sequential {
		if h.prefetchQ == nil {
			return fmt.Errorf("prefetch queue unavailable")
		}
		h.prefetchQ <- prefetchRequest{path: path, size: entry.Size}
		return nil
	}
	if err := h.downloadMgr.Prefetch(path, entry.Size); err != nil {
		return err
	}
	return nil
}

func (h *mountHandler) startPrefetchWorker() {
	if h.downloadMgr == nil || h.prefetchQ == nil {
		return
	}
	h.prefetchWG.Add(1)
	go func() {
		defer h.prefetchWG.Done()
		for req := range h.prefetchQ {
			if err := h.downloadMgr.Prefetch(req.path, req.size); err != nil {
				slog.Warn("prefetch queue: start failed", "path", req.path, "err", err)
				continue
			}
			if err := h.downloadMgr.WaitForRange(req.path, 0, req.size); err != nil {
				slog.Warn("prefetch queue: wait failed", "path", req.path, "err", err)
			}
		}
	}()
}

func (h *mountHandler) stopPrefetchWorker() {
	h.prefetchStop.Do(func() {
		if h.prefetchQ != nil {
			close(h.prefetchQ)
		}
		h.prefetchWG.Wait()
	})
}

func (h *mountHandler) HandleEvict(path string) error {
	if path == "" {
		return fmt.Errorf("evict: missing path")
	}
	return cache.EvictPath(h.cl, path)
}

func (h *mountHandler) HandleDownloads(path string) (ipc.DownloadStatusResponse, error) {
	resp := ipc.DownloadStatusResponse{Entries: []ipc.DownloadStatusEntry{}}
	if h.downloadMgr == nil {
		return resp, nil
	}

	for _, s := range h.downloadMgr.Snapshots(path) {
		resp.Entries = append(resp.Entries, ipc.DownloadStatusEntry{
			Path:       s.Path,
			State:      s.State,
			Downloaded: s.Downloaded,
			TotalSize:  s.TotalSize,
			Done:       s.Done,
			Err:        s.Err,
		})
	}

	if path != "" && len(resp.Entries) == 0 {
		e, err := h.cl.DB().GetFile(path)
		if err != nil {
			return resp, err
		}
		if e != nil {
			downloaded := int64(0)
			d := false
			if e.State == cache.StateClean {
				downloaded = e.Size
				d = true
			}
			resp.Entries = append(resp.Entries, ipc.DownloadStatusEntry{
				Path:       e.Path,
				State:      string(e.State),
				Downloaded: downloaded,
				TotalSize:  e.Size,
				Done:       d,
			})
		}
	}

	return resp, nil
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

	mountCmd.Flags().Var((*byteSizeValue)(&mountCacheSize), "cache-size", "Maximum total cache size; evict clean files when exceeded (e.g. 10G, 500M); 0 = unlimited")

	mountCmd.Flags().Var((*durationValue)(&mountCacheMaxAge), "cache-max-age", "Evict clean files not accessed for this long (e.g. 7d=168h, 30d=720h); 0 = disabled")

	mountCmd.Flags().Var((*byteSizeValue)(&mountCacheMinFreeSpace), "cache-min-free-space", "Minimum free space to maintain on the cache filesystem; evict clean files when below this threshold (e.g. 1G, 500M); 0 = disabled")

	mountCmd.Flags().BoolVar(&mountInstallService, "install-service", false, "Install OS service (systemd/launchd) to auto-start on login and exit")
	mountCmd.Flags().BoolVar(&mountUninstallService, "uninstall-service", false, "Remove previously installed OS service and exit")
	mountDaemonFd = -1
	mountCmd.Flags().IntVar(&mountDaemonFd, "daemon-fd", -1, "")
	_ = mountCmd.Flags().MarkHidden("daemon-fd")
}
