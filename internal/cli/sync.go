package cli

import (
	"fmt"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var syncForce bool

var syncCmd = &cobra.Command{
	Use:   "sync <source>",
	Short: "Trigger an immediate sync cycle",
	Long: `Trigger an immediate sync cycle for a running rvfs mount.

  rvfs sync gdrive:Documents
  rvfs sync gdrive:Documents --force   # clear retry backoff timers first`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteID, sockPath, cl, err := resolveSource(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()
		_ = remoteID

		pending, _ := cl.DB().CountPendingOps()
		conflicts, _ := cl.DB().CountConflicts()

		if syncForce {
			if resetErr := cl.DB().ResetRetryAfter(); resetErr != nil {
				printWarning(cmd.ErrOrStderr(), "could not clear retry timers: %v", resetErr)
			} else {
				printSuccess(cmd.OutOrStdout(), "retry timers cleared")
			}
		}

		c, dialErr := ipc.Dial(sockPath)
		if dialErr != nil {
			return fmt.Errorf("mount not running (could not connect to socket): %w", dialErr)
		}
		defer c.Close()

		if err := c.Sync(syncForce); err != nil {
			return err
		}
		printSection(cmd.OutOrStdout(), fmt.Sprintf("Sync requested for %s", args[0]))
		printKeyValues(cmd.OutOrStdout(), [][2]string{
			{"Pending:", fmt.Sprintf("%d %s", pending, pluralize(pending, "operation", "operations"))},
			{"Conflicts:", fmt.Sprintf("%d unresolved", conflicts)},
		})
		if pending > 0 {
			printHint(cmd.OutOrStdout(), "run 'rvfs queue %s' if uploads remain stuck after a few seconds", args[0])
		}
		if conflicts > 0 {
			printHint(cmd.OutOrStdout(), "run 'rvfs conflicts %s' to inspect unresolved conflicts", args[0])
		}
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

func init() {
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "Clear retry backoff timers before syncing")
}
