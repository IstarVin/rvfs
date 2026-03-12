package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/remote/gdrive"
	"github.com/spf13/cobra"
)

// ---------- conflicts command ----------

var conflictsCacheDir string

var conflictsCmd = &cobra.Command{
	Use:   "conflicts <source>",
	Short: "List conflicting files for a remote",
	Long: `List all files that have conflicting local and remote edits.

<source> uses the same format as 'rvfs mount': remoteName:subpath
  e.g.  rvfs conflicts gdrive:
        rvfs conflicts myremote:Documents`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteName, _, err := parseSource(args[0])
		if err != nil {
			return err
		}

		cacheDir := conflictsCacheDir
		if cacheDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("home dir: %w", err)
			}
			cacheDir = filepath.Join(home, ".cache", "rvfs")
		}

		cl, err := cache.NewCacheLayer(cacheDir, remoteName)
		if err != nil {
			return fmt.Errorf("open cache: %w", err)
		}
		defer cl.Close()

		conflicts, err := cl.DB().ListConflicts()
		if err != nil {
			return fmt.Errorf("list conflicts: %w", err)
		}

		if len(conflicts) == 0 {
			fmt.Println("No conflicts.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tPATH\tLOCAL MTIME\tREMOTE MTIME")
		for _, c := range conflicts {
			localTime := time.Unix(c.LocalMtime, 0).Format("2006-01-02 15:04:05")
			remoteTime := time.Unix(c.RemoteMtime, 0).Format("2006-01-02 15:04:05")
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", c.ID, c.Path, localTime, remoteTime)
		}
		return w.Flush()
	},
}

// ---------- resolve command ----------

var (
	resolveKeep     string
	resolveAll      bool
	resolveCacheDir string
)

var resolveCmd = &cobra.Command{
	Use:   "resolve <source> [id]",
	Short: "Resolve one or all file conflicts for a remote",
	Long: `Resolve file conflicts detected by the sync engine.

  rvfs resolve gdrive: 1 --keep local    # upload local version
  rvfs resolve gdrive: 1 --keep remote   # download remote version
  rvfs resolve gdrive: 1 --keep both     # keep both (sidecar .conflict.* file)
  rvfs resolve gdrive: --all --keep both # resolve every conflict

--keep values: local, remote, both

'local'  — mark the file dirty so the sync engine uploads local changes on the
           next poll cycle, overwriting the remote version.
'remote' — download the current remote version, overwriting local changes.
'both'   — keep the local copy and download the remote version to a sidecar
           file named <path>.conflict.<timestamp>.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteName, _, err := parseSource(args[0])
		if err != nil {
			return err
		}

		if resolveKeep == "" {
			return fmt.Errorf("--keep is required: choose local, remote, or both")
		}
		switch resolveKeep {
		case "local", "remote", "both":
		default:
			return fmt.Errorf("invalid --keep %q: must be local, remote, or both", resolveKeep)
		}

		cacheDir := resolveCacheDir
		if cacheDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("home dir: %w", err)
			}
			cacheDir = filepath.Join(home, ".cache", "rvfs")
		}

		if !resolveAll && len(args) < 2 {
			return fmt.Errorf("conflict ID required unless --all is specified")
		}

		cl, err := cache.NewCacheLayer(cacheDir, remoteName)
		if err != nil {
			return fmt.Errorf("open cache: %w", err)
		}
		defer cl.Close()

		// Collect the set of conflicts to resolve.
		var conflicts []*cache.ConflictEntry
		if resolveAll {
			conflicts, err = cl.DB().ListConflicts()
			if err != nil {
				return fmt.Errorf("list conflicts: %w", err)
			}
		} else {
			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid conflict ID %q: %w", args[1], err)
			}
			ce, err := cl.DB().GetConflict(id)
			if err != nil {
				return fmt.Errorf("get conflict: %w", err)
			}
			if ce == nil {
				return fmt.Errorf("conflict ID %d not found", id)
			}
			conflicts = []*cache.ConflictEntry{ce}
		}

		if len(conflicts) == 0 {
			fmt.Println("No conflicts to resolve.")
			return nil
		}

		// For remote/both strategies we need a live adapter.
		var adapter interface {
			Get(path string, dest io.Writer) error
		}
		if resolveKeep == "remote" || resolveKeep == "both" {
			adapter, err = buildAdapter(remoteName, cacheDir, cl)
			if err != nil {
				return fmt.Errorf("connect to remote: %w", err)
			}
		}

		for _, ce := range conflicts {
			if err := applyResolution(cl, adapter, ce, resolveKeep); err != nil {
				fmt.Fprintf(os.Stderr, "warning: resolve %q: %v\n", ce.Path, err)
				continue
			}
			fmt.Printf("Resolved: %s\n", ce.Path)
		}
		return nil
	},
}

// applyResolution applies the chosen strategy to a single conflict entry.
func applyResolution(cl *cache.CacheLayer, adapter interface {
	Get(path string, dest io.Writer) error
}, ce *cache.ConflictEntry, keep string) error {
	switch keep {
	case "local":
		// Re-queue for upload; next engine cycle will upload.
		if err := cl.DB().SetState(ce.Path, cache.StateDirty); err != nil {
			return fmt.Errorf("set dirty: %w", err)
		}

	case "remote":
		// Download remote, overwrite local cache file.
		entry, err := cl.DB().GetFile(ce.Path)
		if err != nil || entry == nil {
			return fmt.Errorf("file not found in DB")
		}
		diskPath := cl.DiskPath(ce.Path)
		if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		f, err := os.Create(diskPath)
		if err != nil {
			return fmt.Errorf("create: %w", err)
		}
		if dlErr := adapter.Get(ce.Path, f); dlErr != nil {
			f.Close()
			os.Remove(diskPath)
			return fmt.Errorf("download: %w", dlErr)
		}
		f.Close()
		entry.State = cache.StateClean
		entry.RemoteMtime = ce.RemoteMtime
		entry.LocalMtime = ce.RemoteMtime
		entry.SyncError = ""
		if err := cl.DB().PutFile(entry); err != nil {
			return fmt.Errorf("update DB: %w", err)
		}

	case "both":
		// Ensure sidecar exists; if not, download it now.
		conflictRel := fmt.Sprintf("%s.conflict.%d", ce.Path, ce.RemoteMtime)
		diskConflictPath := cl.DiskPath(conflictRel)
		if _, statErr := os.Stat(diskConflictPath); os.IsNotExist(statErr) {
			if err := os.MkdirAll(filepath.Dir(diskConflictPath), 0755); err != nil {
				return fmt.Errorf("mkdir sidecar: %w", err)
			}
			f, err := os.Create(diskConflictPath)
			if err != nil {
				return fmt.Errorf("create sidecar: %w", err)
			}
			if dlErr := adapter.Get(ce.Path, f); dlErr != nil {
				f.Close()
				os.Remove(diskConflictPath)
				return fmt.Errorf("download sidecar: %w", dlErr)
			}
			f.Close()
			entry, err := cl.DB().GetFile(ce.Path)
			if err != nil {
				return fmt.Errorf("get file: %w", err)
			}
			mode := uint32(0100644)
			if entry != nil {
				mode = entry.Mode
			}
			// Stat the sidecar to get actual size.
			info, _ := os.Stat(diskConflictPath)
			var sz int64
			if info != nil {
				sz = info.Size()
			}
			_ = cl.DB().PutFile(&cache.FileEntry{
				Path:        conflictRel,
				IsDir:       false,
				Size:        sz,
				Mode:        mode,
				RemoteMtime: ce.RemoteMtime,
				LocalMtime:  ce.RemoteMtime,
				CachePath:   diskConflictPath,
				State:       cache.StateClean,
			})
		}
		// Keep local dirty so it gets uploaded; clear the conflict state.
		if err := cl.DB().SetState(ce.Path, cache.StateDirty); err != nil {
			return fmt.Errorf("set dirty: %w", err)
		}
	}

	return cl.DB().RemoveConflict(ce.ID)
}

// buildAdapter creates a gdrive remote adapter for the given remote name.
func buildAdapter(remoteName, _ string, cl *cache.CacheLayer) (*gdrive.GDriveAdapter, error) {
	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	rc, exists := cfg.Remotes[remoteName]
	if !exists {
		return nil, fmt.Errorf("remote %q not found in config", remoteName)
	}
	if rc.Type != "gdrive" {
		return nil, fmt.Errorf("unsupported remote type %q", rc.Type)
	}

	tokenPath := config.TokenPath(remoteName)
	return gdrive.New(rc.ClientID, rc.ClientSecret, tokenPath, rc.RootPath, cl.DB())
}

// parseSource splits a "<remoteName>:<subPath>" source string.
// The subPath may be empty.
func parseSource(source string) (remoteName, subPath string, err error) {
	before, after, ok := strings.Cut(source, ":")
	if !ok {
		return "", "", fmt.Errorf("invalid source %q: expected format remoteName:subpath", source)
	}
	return before, after, nil
}

func init() {
	conflictsCmd.Flags().StringVar(&conflictsCacheDir, "cache-dir", "", "Cache directory (default ~/.cache/rvfs)")

	resolveCmd.Flags().StringVar(&resolveKeep, "keep", "", "Resolution strategy: local, remote, or both (required)")
	resolveCmd.Flags().BoolVar(&resolveAll, "all", false, "Resolve all conflicts")
	resolveCmd.Flags().StringVar(&resolveCacheDir, "cache-dir", "", "Cache directory (default ~/.cache/rvfs)")
}
