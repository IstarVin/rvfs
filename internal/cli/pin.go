package cli

import (
	"fmt"
	"path/filepath"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var pinCmd = &cobra.Command{
	Use:   "pin <source> <path>",
	Short: "Pin a file so it is never evicted from the cache",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, cl, err := resolveSource(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()

		rel := filepath.Clean(args[1])
		if err := cl.DB().SetPinned(rel, true); err != nil {
			return fmt.Errorf("pin %q: %w", rel, err)
		}
		fmt.Printf("Pinned: %s\n", rel)
		return nil
	},
}

var unpinCmd = &cobra.Command{
	Use:   "unpin <source> <path>",
	Short: "Unpin a file, allowing it to be evicted from the cache",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, cl, err := resolveSource(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()

		rel := filepath.Clean(args[1])
		if err := cl.DB().SetPinned(rel, false); err != nil {
			return fmt.Errorf("unpin %q: %w", rel, err)
		}
		fmt.Printf("Unpinned: %s\n", rel)
		return nil
	},
}

var pinsCmd = &cobra.Command{
	Use:   "pins [source]",
	Short: "List pinned paths",
	Long: `List all pinned paths for a remote.

  rvfs pins gdrive:Documents`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return listAllPins()
		}
		return listPins(args[0])
	},
}

func listPins(source string) error {
	_, _, cl, err := resolveSource(source)
	if err != nil {
		return err
	}
	defer cl.Close()

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
