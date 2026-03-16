package cli

import (
	"fmt"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var downloadsJSON bool

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
		if downloadsJSON {
			return writeJSON(cmd.OutOrStdout(), resp)
		}
		if len(resp.Entries) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No matching downloads.")
			return nil
		}

		active := 0
		queued := 0
		failed := 0
		completed := 0
		for _, e := range resp.Entries {
			switch {
			case e.Err != "":
				failed++
			case e.State == "queued":
				queued++
			case e.Done:
				completed++
			default:
				active++
			}
		}

		printSection(cmd.OutOrStdout(), fmt.Sprintf("Downloads for %s", args[0]))
		fprintf(cmd.OutOrStdout(), "%d tracked, %d queued, %d active, %d complete, %d with errors\n", len(resp.Entries), queued, active, completed, failed)

		rows := make([][]string, 0, len(resp.Entries))
		for _, e := range resp.Entries {
			progress := "-"
			if e.State == "queued" {
				progress = mutedStyle.Render("queued")
			} else if e.TotalSize > 0 {
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
				progress = okStyle.Render("done")
			}
			state := e.State
			if e.Err != "" {
				progress = errorStyle.Render("error")
				state = warnStyle.Render(e.State)
				bytes = ellipsize(e.Err, 38)
			}
			rows = append(rows, []string{e.Path, state, progress, bytes})
		}

		renderTable(cmd.OutOrStdout(), []tableColumn{
			{Title: "PATH", Width: 40},
			{Title: "STATE", Width: 14},
			{Title: "PROGRESS", Width: 12},
			{Title: "DETAIL", Width: 38},
		}, rows)
		if failed > 0 {
			printHint(cmd.OutOrStdout(), "retry the affected path by reopening it or remounting if the errors persist")
		}
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

func init() {
	downloadsCmd.Flags().BoolVar(&downloadsJSON, "json", false, "Output machine-readable JSON")
}
