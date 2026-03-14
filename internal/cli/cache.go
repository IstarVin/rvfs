package cli

import (
	"fmt"
	"strings"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/spf13/cobra"
)

var cacheCleanIncludePinned bool

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage local cache data",
}

var cacheCleanCmd = &cobra.Command{
	Use:   "clean <source|mount-path>",
	Short: "Evict clean files from local cache",
	Long: `Evict clean files from local cache for a source or active mount path.

By default pinned files are kept. Use --include-pinned to evict pinned
clean files as well. Dirty/syncing/downloading/conflict files are never
evicted by this command.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cl, target, err := resolveCacheTarget(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()

		entries, err := cl.DB().ListCleanFiles(cacheCleanIncludePinned)
		if err != nil {
			return fmt.Errorf("list cache cleanup candidates: %w", err)
		}

		attempted := len(entries)
		evicted := 0
		failed := 0
		var freed int64

		for _, e := range entries {
			if err := cache.EvictPath(cl, e.Path); err != nil {
				failed++
				continue
			}
			evicted++
			freed += e.Size
		}

		printSection(cmd.OutOrStdout(), "Cache cleaned")
		printKeyValues(cmd.OutOrStdout(), [][2]string{
			{"Target:", target},
			{"Include pinned:", fmt.Sprintf("%t", cacheCleanIncludePinned)},
			{"Candidates:", fmt.Sprintf("%d", attempted)},
			{"Evicted:", fmt.Sprintf("%d", evicted)},
			{"Freed:", humanBytes(freed)},
		})
		if failed > 0 {
			printWarning(cmd.ErrOrStderr(), "failed to evict %d %s", failed, pluralize(failed, "file", "files"))
		}
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

func resolveCacheTarget(target string) (*cache.CacheLayer, string, error) {
	if strings.Contains(target, ":") {
		_, _, cl, err := resolveSource(target)
		if err != nil {
			return nil, "", err
		}
		return cl, target, nil
	}
	_, _, cl, _, err := resolveMountPath(target)
	if err != nil {
		return nil, "", err
	}
	return cl, target, nil
}

func init() {
	cacheCleanCmd.Flags().BoolVar(&cacheCleanIncludePinned, "include-pinned", false, "Also evict pinned clean files")
	cacheCmd.AddCommand(cacheCleanCmd)
}
