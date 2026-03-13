package cli

import (
	"fmt"
	"path/filepath"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var pinCmd = &cobra.Command{
	Use:   "pin (<source> <path> | <mount-path>)",
	Short: "Pin a file so it is never evicted from the cache",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var cl *cache.CacheLayer
		var sockPath string
		var rel string
		var err error

		if len(args) == 1 {
			_, sockPath, cl, rel, err = resolveMountPath(args[0])
			if err != nil {
				return err
			}
		} else {
			_, sockPath, cl, err = resolveSource(args[0])
			if err != nil {
				return err
			}
			rel = filepath.Clean(args[1])
		}
		defer cl.Close()

		if err := cl.DB().SetPinned(rel, true); err != nil {
			return fmt.Errorf("pin %q: %w", rel, err)
		}

		prefetchStatus := "metadata-only"
		if c, dialErr := ipc.Dial(sockPath); dialErr == nil {
			defer c.Close()
			if err := c.Prefetch(rel); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: pinned but prefetch failed for %s: %v\n", rel, err)
			} else {
				prefetchStatus = "prefetch started"
			}
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: pinned but mount is not reachable; cache prefetch not started\n")
		}

		fmt.Printf("Pinned: %s (%s)\n", rel, prefetchStatus)
		return nil
	},
}

var unpinCmd = &cobra.Command{
	Use:   "unpin (<source> <path> | <mount-path>)",
	Short: "Unpin a file, allowing it to be evicted from the cache",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		var cl *cache.CacheLayer
		var sockPath string
		var rel string
		var err error

		if len(args) == 1 {
			_, sockPath, cl, rel, err = resolveMountPath(args[0])
			if err != nil {
				return err
			}
		} else {
			_, sockPath, cl, err = resolveSource(args[0])
			if err != nil {
				return err
			}
			rel = filepath.Clean(args[1])
		}
		defer cl.Close()

		if err := cl.DB().SetPinned(rel, false); err != nil {
			return fmt.Errorf("unpin %q: %w", rel, err)
		}

		evictStatus := "metadata-only"
		if c, dialErr := ipc.Dial(sockPath); dialErr == nil {
			defer c.Close()
			if err := c.Evict(rel); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: unpinned but cache eviction failed for %s: %v\n", rel, err)
			} else {
				evictStatus = "cache evicted"
			}
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: unpinned but mount is not reachable; immediate cache eviction skipped\n")
		}

		fmt.Printf("Unpinned: %s (%s)\n", rel, evictStatus)
		return nil
	},
}

var pinsCmd = &cobra.Command{
	Use:   "pins [source | mount-path]",
	Short: "List pinned paths",
	Long: `List all pinned paths for a remote.

  rvfs pins gdrive:Documents
  rvfs pins /mnt/gdrive`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return listAllPins()
		}
		if filepath.IsAbs(args[0]) {
			return listPinsByMount(args[0])
		}
		return listPins(args[0])
	},
}

func listPinsByMount(path string) error {
	_, _, cl, _, err := resolveMountPath(path)
	if err != nil {
		return err
	}
	defer cl.Close()
	return printPins(cl)
}

func listPins(source string) error {
	_, _, cl, err := resolveSource(source)
	if err != nil {
		return err
	}
	defer cl.Close()
	return printPins(cl)
}

func printPins(cl *cache.CacheLayer) error {

	pins, err := cl.DB().ListPinned()
	if err != nil {
		return fmt.Errorf("list pinned: %w", err)
	}
	if len(pins) == 0 {
		fmt.Println("No pinned paths.")
		return nil
	}

	hdr := lipgloss.NewStyle().Bold(true).Underline(true)
	fmt.Printf("%s  %s\n", hdr.Width(6).Render("TYPE"), hdr.Render("PATH"))
	for _, e := range pins {
		t := "file"
		if e.IsDir {
			t = "dir"
		}
		fmt.Printf("%-8s %s\n", t, e.Path)
	}
	return nil
}

func listAllPins() error {
	cacheDir := getCacheDir()
	// Walk one level deep: each subdirectory is a remoteID.
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*", "meta.db"))
	if err != nil || len(entries) == 0 {
		fmt.Println("No remotes found in cache directory.")
		return nil
	}
	for _, dbPath := range entries {
		remoteID := filepath.Base(filepath.Dir(dbPath))
		source := remoteID + ":"
		if err := listPins(source); err != nil {
			fmt.Printf("  (error reading %s: %v)\n", remoteID, err)
		}
	}
	return nil
}
