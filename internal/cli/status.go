package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status [source]",
	Short: "Show status of a mounted remote",
	Long: `Show mount info, cache usage, pending operations and conflicts.

  rvfs status gdrive:Documents

If no source is given, all active mounts are queried.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return showStatus(args[0])
		}
		return showAllStatus()
	},
}

func showStatus(source string) error {
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
			printStatus(resp)
			return nil
		}
	}

	// Fallback: read directly from the SQLite DB (mount not running).
	pending, _ := cl.DB().CountPendingOps()
	conflicts, _ := cl.DB().CountConflicts()
	printStatus(ipc.StatusResponse{
		Source:     source,
		Mountpoint: "(not mounted)",
		Online:     false,
		CacheUsed:  0,
		CacheTotal: 0,
		Pending:    pending,
		Conflicts:  conflicts,
	})
	_ = remoteID
	return nil
}

func showAllStatus() error {
	_ = getCacheDir()

	// Enumerate *.sock files to find active mounts.
	sockDir := ipc.SockDir()

	ents, err := os.ReadDir(sockDir)
	if err != nil || len(ents) == 0 {
		fmt.Println("No active mounts found.")
		return nil
	}
	found := false
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
		printStatus(resp)
		found = true
	}
	if !found {
		fmt.Println("No active mounts found.")
	}
	return nil
}

// Lipgloss styles.
var (
	labelStyle = lipgloss.NewStyle().Bold(true).Width(11)
	valueStyle = lipgloss.NewStyle()
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	okStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
)

func printStatus(r ipc.StatusResponse) {
	onlineStr := okStyle.Render("ONLINE")
	if !r.Online {
		onlineStr = warnStyle.Render("OFFLINE")
	}

	cacheStr := "—"
	if r.CacheTotal > 0 {
		pct := float64(r.CacheUsed) / float64(r.CacheTotal) * 100
		cacheStr = fmt.Sprintf("%s / %s (%.0f%%)",
			humanBytes(r.CacheUsed), humanBytes(r.CacheTotal), pct)
	} else if r.CacheUsed > 0 {
		cacheStr = humanBytes(r.CacheUsed)
	}

	rows := [][2]string{
		{"Mount:", r.Mountpoint},
		{"Remote:", r.Source},
		{"State:", onlineStr},
		{"Cache:", cacheStr},
		{"Pending:", fmt.Sprintf("%d files to upload", r.Pending)},
		{"Conflicts:", fmt.Sprintf("%d unresolved", r.Conflicts)},
	}
	for _, row := range rows {
		fmt.Printf("%s %s\n", labelStyle.Render(row[0]), valueStyle.Render(row[1]))
	}
	fmt.Println()
}

func humanBytes(b int64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
		TB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
