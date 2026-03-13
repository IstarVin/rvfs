package cli

import (
	"fmt"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var downloadsCmd = &cobra.Command{
	Use:   "downloads <source> [path]",
	Short: "Show cache download status",
	Long: `Show active or known download status for a mounted remote.

  rvfs downloads gdrive:Documents
  rvfs downloads gdrive:Documents videos/movie.mp4`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, sockPath, cl, err := resolveSource(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()

		path := ""
		if len(args) == 2 {
			path = args[1]
		}

		c, dialErr := ipc.Dial(sockPath)
		if dialErr != nil {
			return fmt.Errorf("mount not running (could not connect to socket): %w", dialErr)
		}
		defer c.Close()

		resp, err := c.Downloads(path)
		if err != nil {
			return err
		}
		if len(resp.Entries) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No matching downloads.")
			return nil
		}

		hdr := lipgloss.NewStyle().Bold(true).Underline(true)
		fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s  %s\n",
			hdr.Width(40).Render("PATH"),
			hdr.Width(12).Render("STATE"),
			hdr.Width(18).Render("PROGRESS"),
			hdr.Render("BYTES"),
		)
		for _, e := range resp.Entries {
			progress := "-"
			if e.TotalSize > 0 {
				pct := float64(e.Downloaded) / float64(e.TotalSize) * 100
				if pct < 0 {
					pct = 0
				}
				if pct > 100 {
					pct = 100
				}
				progress = fmt.Sprintf("%.0f%%", pct)
			}
			bytes := fmt.Sprintf("%s / %s", humanBytes(e.Downloaded), humanBytes(e.TotalSize))
			if e.Done {
				progress = "done"
			}
			if e.Err != "" {
				progress = "error"
				bytes = e.Err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-12s  %-18s  %s\n", e.Path, e.State, progress, bytes)
		}
		return nil
	},
}
