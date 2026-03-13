package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var (
	pinsTree bool
	pinsJSON bool
)

var pinCmd = &cobra.Command{
	Use:   "pin (<source> <path> | <mount-path>)",
	Short: "Pin a file or directory so it is never evicted from the cache",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, sockPath, cl, rel, err := resolvePinTarget(args)
		if err != nil {
			return err
		}
		defer cl.Close()

		pinPaths, filePaths, targetIsDir, err := pinTargets(cl, rel)
		if err != nil {
			return err
		}

		if err := cl.DB().SetPinnedMany(pinPaths, true); err != nil {
			return fmt.Errorf("pin %q: %w", rel, err)
		}

		prefetchStatus := "metadata-only"
		if c, dialErr := ipc.Dial(sockPath); dialErr == nil {
			defer c.Close()
			started := 0
			failed := make([]string, 0)
			for _, p := range filePaths {
				var err error
				if targetIsDir {
					err = c.PrefetchSequential(p)
				} else {
					err = c.Prefetch(p)
				}
				if err != nil {
					failed = append(failed, fmt.Sprintf("%s (%v)", p, err))
					continue
				}
				started++
			}
			switch {
			case len(filePaths) == 0:
				prefetchStatus = "no files to prefetch"
			case len(failed) == 0:
				if targetIsDir {
					prefetchStatus = fmt.Sprintf("prefetch queued for %d file(s)", started)
				} else {
					prefetchStatus = fmt.Sprintf("prefetch started for %d file(s)", started)
				}
			default:
				if targetIsDir {
					prefetchStatus = fmt.Sprintf("prefetch queued for %d/%d file(s)", started, len(filePaths))
				} else {
					prefetchStatus = fmt.Sprintf("prefetch started for %d/%d file(s)", started, len(filePaths))
				}
				printWarning(cmd.ErrOrStderr(), "pinned, but prefetch failed for %d file(s): %s", len(failed), strings.Join(failed, "; "))
			}
		} else {
			if len(filePaths) > 0 {
				printWarning(cmd.ErrOrStderr(), "pinned, but the mount is not reachable; cache prefetch did not start")
			}
		}

		printSection(cmd.OutOrStdout(), "Pinned")
		printKeyValues(cmd.OutOrStdout(), [][2]string{
			{"Path:", rel},
			{"Pinned:", fmt.Sprintf("%d %s", len(pinPaths), pluralize(len(pinPaths), "path", "paths"))},
			{"Fetch:", prefetchStatus},
		})
		if targetIsDir {
			printHint(cmd.OutOrStdout(), "directory prefetch is queued sequentially to avoid flooding the downloader")
		}
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

var unpinCmd = &cobra.Command{
	Use:   "unpin (<source> <path> | <mount-path>)",
	Short: "Unpin a file or directory, allowing it to be evicted from the cache",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, sockPath, cl, rel, err := resolvePinTarget(args)
		if err != nil {
			return err
		}
		defer cl.Close()

		pinPaths, filePaths, _, err := pinTargets(cl, rel)
		if err != nil {
			return err
		}

		if err := cl.DB().SetPinnedMany(pinPaths, false); err != nil {
			return fmt.Errorf("unpin %q: %w", rel, err)
		}

		evictStatus := "metadata-only"
		if c, dialErr := ipc.Dial(sockPath); dialErr == nil {
			defer c.Close()
			evicted := 0
			failed := make([]string, 0)
			for _, p := range filePaths {
				if err := c.Evict(p); err != nil {
					failed = append(failed, fmt.Sprintf("%s (%v)", p, err))
					continue
				}
				evicted++
			}
			switch {
			case len(filePaths) == 0:
				evictStatus = "no files to evict"
			case len(failed) == 0:
				evictStatus = fmt.Sprintf("cache evicted for %d file(s)", evicted)
			default:
				evictStatus = fmt.Sprintf("cache evicted for %d/%d file(s)", evicted, len(filePaths))
				printWarning(cmd.ErrOrStderr(), "unpinned, but cache eviction failed for %d file(s): %s", len(failed), strings.Join(failed, "; "))
			}
		} else {
			if len(filePaths) > 0 {
				printWarning(cmd.ErrOrStderr(), "unpinned, but the mount is not reachable; immediate cache eviction was skipped")
			}
		}

		printSection(cmd.OutOrStdout(), "Unpinned")
		printKeyValues(cmd.OutOrStdout(), [][2]string{
			{"Path:", rel},
			{"Unpinned:", fmt.Sprintf("%d %s", len(pinPaths), pluralize(len(pinPaths), "path", "paths"))},
			{"Cache:", evictStatus},
		})
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

func resolvePinTarget(args []string) (string, string, *cache.CacheLayer, string, error) {
	if len(args) == 1 {
		return resolveMountPath(args[0])
	}
	remote, sockPath, cl, err := resolveSource(args[0])
	if err != nil {
		return "", "", nil, "", err
	}
	return remote, sockPath, cl, filepath.Clean(args[1]), nil
}

func pinTargets(cl *cache.CacheLayer, rel string) ([]string, []string, bool, error) {
	entry, err := cl.DB().GetFile(rel)
	if err != nil {
		return nil, nil, false, fmt.Errorf("lookup %q: %w", rel, err)
	}
	if entry == nil {
		return nil, nil, false, fmt.Errorf("path %q not found", rel)
	}

	paths := []string{rel}
	filePaths := make([]string, 0)
	if !entry.IsDir {
		filePaths = append(filePaths, rel)
		return paths, filePaths, false, nil
	}

	desc, err := cl.DB().ListDescendants(rel)
	if err != nil {
		return nil, nil, true, err
	}
	for _, e := range desc {
		paths = append(paths, e.Path)
		if !e.IsDir {
			filePaths = append(filePaths, e.Path)
		}
	}

	return paths, filePaths, true, nil
}

var pinsCmd = &cobra.Command{
	Use:   "pins [source | mount-path]",
	Short: "List pinned paths",
	Long: `List all pinned paths for a remote.

  rvfs pins gdrive:Documents
  rvfs pins /mnt/gdrive
  rvfs pins gdrive:Documents --tree`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return listAllPins(cmd.OutOrStdout())
		}
		if filepath.IsAbs(args[0]) {
			return listPinsByMount(cmd.OutOrStdout(), args[0])
		}
		return listPins(cmd.OutOrStdout(), args[0])
	},
}

func listPinsByMount(w io.Writer, path string) error {
	_, _, cl, _, err := resolveMountPath(path)
	if err != nil {
		return err
	}
	defer cl.Close()
	return printPins(w, cl)
}

func listPins(w io.Writer, source string) error {
	_, _, cl, err := resolveSource(source)
	if err != nil {
		return err
	}
	defer cl.Close()
	return printPins(w, cl)
}

func printPins(w io.Writer, cl *cache.CacheLayer) error {
	pins, err := cl.DB().ListPinned()
	if err != nil {
		return fmt.Errorf("list pinned: %w", err)
	}
	if len(pins) == 0 {
		fprintln(w, "No pinned paths.")
		return nil
	}

	out, err := compactPinnedEntries(cl.DB(), pins)
	if err != nil {
		return err
	}
	if pinsJSON {
		return writeJSON(w, out)
	}

	if pinsTree {
		expanded, err := expandForTree(cl.DB(), out)
		if err != nil {
			return err
		}
		printSection(w, "Pinned paths")
		for _, line := range renderPinsTree(expanded) {
			fprintln(w, line)
		}
		fprintln(w)
		return nil
	}

	printSection(w, "Pinned paths")
	for _, e := range out {
		fprintln(w, formatPinnedPath(e))
	}
	fprintln(w)
	return nil
}

type pinnedOutput struct {
	Path  string
	IsDir bool
}

func compactPinnedEntries(db *cache.MetadataDB, pins []*cache.FileEntry) ([]pinnedOutput, error) {
	display := make(map[string]pinnedOutput, len(pins))
	for _, e := range pins {
		display[e.Path] = pinnedOutput{Path: e.Path, IsDir: e.IsDir}
	}

	candidates, err := collapseCandidates(db, pins)
	if err != nil {
		return nil, err
	}
	collapsed := make(map[string]struct{})
	for _, dir := range candidates {
		if hasCollapsedAncestor(dir.Path, collapsed) {
			continue
		}
		display[dir.Path] = dir
		collapsed[dir.Path] = struct{}{}
		prefix := dir.Path + "/"
		for p := range display {
			if strings.HasPrefix(p, prefix) {
				delete(display, p)
			}
		}
	}

	out := make([]pinnedOutput, 0, len(display))
	for _, e := range display {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func hasCollapsedAncestor(path string, collapsed map[string]struct{}) bool {
	dir := filepath.Dir(path)
	for dir != "." && dir != "/" {
		if _, ok := collapsed[dir]; ok {
			return true
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	return false
}

func collapseCandidates(db *cache.MetadataDB, pins []*cache.FileEntry) ([]pinnedOutput, error) {
	seenDirs := make(map[string]struct{})
	for _, e := range pins {
		if e.IsDir {
			seenDirs[e.Path] = struct{}{}
		}
		dir := filepath.Dir(e.Path)
		for dir != "." && dir != "/" {
			seenDirs[dir] = struct{}{}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
			dir = next
		}
	}

	dirs := make([]string, 0, len(seenDirs))
	for d := range seenDirs {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if len(dirs[i]) == len(dirs[j]) {
			return dirs[i] < dirs[j]
		}
		return len(dirs[i]) < len(dirs[j])
	})

	out := make([]pinnedOutput, 0)
	for _, dir := range dirs {
		entry, err := db.GetFile(dir)
		if err != nil {
			return nil, fmt.Errorf("compact pins: stat %q: %w", dir, err)
		}
		if entry == nil || !entry.IsDir {
			continue
		}

		desc, err := db.ListDescendants(dir)
		if err != nil {
			return nil, fmt.Errorf("compact pins: descendants %q: %w", dir, err)
		}
		files := 0
		allPinned := true
		for _, d := range desc {
			if d.IsDir {
				continue
			}
			files++
			if !d.Pinned {
				allPinned = false
				break
			}
		}
		if files > 0 && allPinned {
			out = append(out, pinnedOutput{Path: dir, IsDir: true})
		}
	}
	return out, nil
}

func expandForTree(db *cache.MetadataDB, entries []pinnedOutput) ([]pinnedOutput, error) {
	seen := make(map[string]struct{})
	result := make([]pinnedOutput, 0, len(entries))
	for _, e := range entries {
		if _, ok := seen[e.Path]; !ok {
			seen[e.Path] = struct{}{}
			result = append(result, e)
		}
		if e.IsDir {
			desc, err := db.ListDescendants(e.Path)
			if err != nil {
				return nil, fmt.Errorf("expand tree %q: %w", e.Path, err)
			}
			for _, d := range desc {
				if _, ok := seen[d.Path]; !ok {
					seen[d.Path] = struct{}{}
					result = append(result, pinnedOutput{Path: d.Path, IsDir: d.IsDir})
				}
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func formatPinnedPath(e pinnedOutput) string {
	if e.IsDir {
		return e.Path + "/"
	}
	return e.Path
}

type treeNode struct {
	name     string
	isDir    bool
	children map[string]*treeNode
}

func renderPinsTree(entries []pinnedOutput) []string {
	root := &treeNode{children: make(map[string]*treeNode)}
	for _, e := range entries {
		parts := strings.Split(e.Path, "/")
		n := root
		for i, p := range parts {
			child, ok := n.children[p]
			if !ok {
				child = &treeNode{name: p, children: make(map[string]*treeNode)}
				n.children[p] = child
			}
			if i < len(parts)-1 {
				child.isDir = true
			}
			n = child
		}
		n.isDir = e.IsDir || len(n.children) > 0
	}

	lines := make([]string, 0)
	var walk func(prefix string, children map[string]*treeNode)
	walk = func(prefix string, children map[string]*treeNode) {
		names := make([]string, 0, len(children))
		for n := range children {
			names = append(names, n)
		}
		sort.Strings(names)

		for i, name := range names {
			last := i == len(names)-1
			branch := "├── "
			nextPrefix := prefix + "│   "
			if last {
				branch = "└── "
				nextPrefix = prefix + "    "
			}

			n := children[name]
			label := n.name
			if n.isDir || len(n.children) > 0 {
				label += "/"
			}
			lines = append(lines, prefix+branch+label)
			if len(n.children) > 0 {
				walk(nextPrefix, n.children)
			}
		}
	}
	walk("", root.children)
	return lines
}

func listAllPins(w io.Writer) error {
	cacheDir := getCacheDir()
	// Walk one level deep: each subdirectory is a remoteID.
	entries, err := filepath.Glob(filepath.Join(cacheDir, "*", "meta.db"))
	if err != nil || len(entries) == 0 {
		fprintln(w, "No remotes found in cache directory.")
		return nil
	}
	sort.Strings(entries)
	for i, dbPath := range entries {
		remoteID := filepath.Base(filepath.Dir(dbPath))
		source := remoteID + ":"
		fprintln(w, source)
		if err := listPins(w, source); err != nil {
			fprintf(w, "  (error reading %s: %v)\n", remoteID, err)
		}
		if i < len(entries)-1 {
			fprintln(w)
		}
	}
	return nil
}

func init() {
	pinsCmd.Flags().BoolVar(&pinsTree, "tree", false, "Show pinned paths as a tree")
	pinsCmd.Flags().BoolVar(&pinsJSON, "json", false, "Output machine-readable JSON")
}
