package cli

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

var queueCmd = &cobra.Command{
	Use:   "queue <source>",
	Short: "Show pending sync operations",
	Long:  `Show the queue of pending upload/delete/rename operations for a mounted remote.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, cl, err := resolveSource(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()

		ops, err := cl.DB().NextPendingOps(1000)
		if err != nil {
			return fmt.Errorf("list pending ops: %w", err)
		}
		if len(ops) == 0 {
			fmt.Println("No pending operations.")
			return nil
		}

		// Header styles.
		hdr := lipgloss.NewStyle().Bold(true).Underline(true)
		cell := lipgloss.NewStyle().PaddingRight(2)

		fmt.Printf("%s  %s  %s  %s  %s\n",
			hdr.Width(8).Render("PRIORITY"),
			hdr.Width(40).Render("PATH"),
			hdr.Width(8).Render("OP"),
			hdr.Width(10).Render("SIZE"),
			hdr.Render("QUEUED"),
		)

		for i, op := range ops {
			size := "—"
			if op.Op == "put" {
				// Attempt to get size from DB.
				if e, err2 := cl.DB().GetFile(op.Path); err2 == nil && e != nil {
					size = humanBytes(e.Size)
				}
			}
			queued := "—"
			if op.QueuedAt > 0 {
				queued = humanDuration(time.Since(time.Unix(op.QueuedAt, 0)))
			}
			fmt.Printf("%s  %s  %s  %s  %s\n",
				cell.Width(8).Render(fmt.Sprintf("%d", i+1)),
				cell.Width(40).Render(op.Path),
				cell.Width(8).Render(op.Op),
				cell.Width(10).Render(size),
				cell.Render(queued+" ago"),
			)
		}
		return nil
	},
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
