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

		if syncForce {
			if resetErr := cl.DB().ResetRetryAfter(); resetErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: reset retry timers: %v\n", resetErr)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Retry timers cleared.")
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
		fmt.Fprintln(cmd.OutOrStdout(), "Sync triggered.")
		return nil
	},
}

func init() {
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "Clear retry backoff timers before syncing")
}
