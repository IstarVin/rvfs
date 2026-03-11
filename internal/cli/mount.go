package cli

import (
	"fmt"
	"os"

	"github.com/IstarVin/rvfs/internal/fuse"
	"github.com/spf13/cobra"
)

var mountDebug bool

var mountCmd = &cobra.Command{
	Use:   "mount <backing-dir> <mountpoint>",
	Short: "Mount a local directory via FUSE",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		backingDir := args[0]
		mountpoint := args[1]

		server, err := fuse.Mount(backingDir, mountpoint, fuse.MountOptions{
			Debug: mountDebug,
		})
		if err != nil {
			return fmt.Errorf("mount: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Mounted %s at %s (press Ctrl-C to unmount)\n", backingDir, mountpoint)
		server.Wait()
		return nil
	},
}

func init() {
	mountCmd.Flags().BoolVar(&mountDebug, "debug", false, "Enable FUSE debug logging")
}
