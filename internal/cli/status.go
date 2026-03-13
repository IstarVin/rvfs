package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var statusJSON bool

var statusCmd = &cobra.Command{
	Use:   "status [source]",
	Short: "Show status of a mounted remote",
	Long: `Show mount info, cache usage, pending operations and conflicts.

  rvfs status gdrive:Documents

If no source is given, all active mounts are queried.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return showStatus(cmd, args[0])
		}
		return showAllStatus(cmd)
	},
}

func showStatus(cmd *cobra.Command, source string) error {
	remoteID, sockPath, cl, err := resolveSource(source)
	if err != nil {
		return err
	}
	defer cl.Close()

	// Try live data from the running mount process first.
	if c, dialErr := ipc.Dial(sockPath); dialErr == nil {
		defer c.Close()
		resp, statusErr := c.Status()
		if statusErr == nil {
			if statusJSON {
				return writeJSON(cmd.OutOrStdout(), resp)
			}
			printStatus(cmd.OutOrStdout(), resp, true)
			return nil
		}
	}

	// Fallback: read directly from the SQLite DB (mount not running).
	pending, _ := cl.DB().CountPendingOps()
	conflicts, _ := cl.DB().CountConflicts()
	resp := ipc.StatusResponse{
		Source:     source,
		Mountpoint: "(not mounted)",
		Online:     false,
		CacheUsed:  0,
		CacheTotal: 0,
		Pending:    pending,
		Conflicts:  conflicts,
	}
	if statusJSON {
		return writeJSON(cmd.OutOrStdout(), resp)
	}
	printStatus(cmd.OutOrStdout(), resp, true)
	_ = remoteID
	return nil
}

func showAllStatus(cmd *cobra.Command) error {
	responses, err := collectAllStatus()
	if err != nil {
		return err
	}
	if len(responses) == 0 {
		fprintln(cmd.OutOrStdout(), "No active mounts found.")
		return nil
	}
	if statusJSON {
		return writeJSON(cmd.OutOrStdout(), responses)
	}
	for i, resp := range responses {
		printStatus(cmd.OutOrStdout(), resp, true)
		if i < len(responses)-1 {
			fprintln(cmd.OutOrStdout())
		}
	}
	return nil
}

func collectAllStatus() ([]ipc.StatusResponse, error) {
	_ = getCacheDir()

	reg, regErr := ipc.OpenMountRegistry()
	if regErr == nil {
		defer reg.Close()
		entries, err := reg.ListAll()
		if err == nil && len(entries) > 0 {
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].Source == entries[j].Source {
					return entries[i].Mountpoint < entries[j].Mountpoint
				}
				return entries[i].Source < entries[j].Source
			})

			responses := make([]ipc.StatusResponse, 0, len(entries))
			for _, entry := range entries {
				c, dialErr := ipc.Dial(entry.SockPath)
				if dialErr != nil {
					continue
				}
				resp, statusErr := c.Status()
				c.Close()
				if statusErr != nil {
					continue
				}
				responses = append(responses, resp)
			}
			if len(responses) > 0 {
				return responses, nil
			}
		}
	}

	sockDir := ipc.SockDir()
	ents, err := os.ReadDir(sockDir)
	if err != nil || len(ents) == 0 {
		return nil, nil
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	responses := make([]ipc.StatusResponse, 0, len(ents))
	for _, ent := range ents {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".sock" {
			continue
		}
		sockPath := filepath.Join(sockDir, ent.Name())
		c, dialErr := ipc.Dial(sockPath)
		if dialErr != nil {
			continue
		}
		resp, statusErr := c.Status()
		c.Close()
		if statusErr != nil {
			continue
		}
		responses = append(responses, resp)
	}
	return responses, nil
}

func printStatus(w io.Writer, r ipc.StatusResponse, showHints bool) {
	onlineStr := statusBadge(r.Online, "ONLINE", "OFFLINE")

	cacheStr := "—"
	if r.CacheMinFreeSpace > 0 && r.CacheFSFree > 0 {
		avail := r.CacheFSFree - r.CacheMinFreeSpace
		if avail < 0 {
			avail = 0
		}
		if r.CacheTotal > 0 {
			// Cap available by the configured max cache size minus what's used.
			remaining := r.CacheTotal - r.CacheUsed
			if remaining < avail {
				avail = remaining
			}
		}
		cacheStr = fmt.Sprintf("%s used, %s available to fill",
			humanBytes(r.CacheUsed), humanBytes(avail))
	} else if r.CacheTotal > 0 {
		pct := float64(r.CacheUsed) / float64(r.CacheTotal) * 100
		cacheStr = fmt.Sprintf("%s / %s (%.0f%%)",
			humanBytes(r.CacheUsed), humanBytes(r.CacheTotal), pct)
	} else if r.CacheUsed > 0 {
		cacheStr = humanBytes(r.CacheUsed)
	}

	pendingLabel := fmt.Sprintf("%d %s waiting", r.Pending, pluralize(r.Pending, "upload", "uploads"))
	if r.Pending == 0 {
		pendingLabel = okStyle.Render("Up to date")
	}
	conflictLabel := fmt.Sprintf("%d unresolved", r.Conflicts)
	if r.Conflicts == 0 {
		conflictLabel = okStyle.Render("None")
	}

	printSection(w, r.Source)
	rows := [][2]string{
		{"Mount:", r.Mountpoint},
		{"Remote:", r.Source},
		{"State:", onlineStr},
		{"Cache:", cacheStr},
		{"Pending:", pendingLabel},
		{"Conflicts:", conflictLabel},
	}
	printKeyValues(w, rows)
	if showHints {
		switch {
		case !r.Online && r.Mountpoint != "(not mounted)":
			printHint(w, "cached files remain available; sync will resume when connectivity returns")
		case r.Pending > 0:
			printHint(w, "run 'rvfs queue %s' to inspect pending operations", r.Source)
		}
		if r.Conflicts > 0 {
			printHint(w, "run 'rvfs conflicts %s' to review and resolve conflicts", r.Source)
		}
	}
	fprintln(w)
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output machine-readable JSON")
}
