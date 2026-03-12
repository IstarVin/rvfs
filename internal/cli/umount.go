package cli

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var umountCmd = &cobra.Command{
	Use:   "umount <mountpoint>",
	Short: "Unmount a FUSE filesystem",
	Long: `Gracefully unmount an rvfs FUSE filesystem.

If there are pending uploads the command will warn but still unmount.

  rvfs umount ~/Documents`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mountpoint := args[0]

		// Warn if there are pending ops. We can't derive the source/remoteID
		// from just the mountpoint without a registry, so we skip the pending
		// count here and let the user know they can check with `rvfs queue`.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Warning: check `rvfs queue` first to ensure all uploads are complete.\n")
		_ = ipc.SockPath // ensure import used

		return unmount(mountpoint)
	},
}

func unmount(mountpoint string) error {
	var out []byte
	var err error

	switch runtime.GOOS {
	case "linux":
		// Try fusermount3 first (FUSE3), then fusermount (FUSE2).
		out, err = exec.Command("fusermount3", "-u", mountpoint).CombinedOutput()
		if err != nil {
			out, err = exec.Command("fusermount", "-u", mountpoint).CombinedOutput()
		}
	case "darwin":
		out, err = exec.Command("umount", mountpoint).CombinedOutput()
	default:
		out, err = exec.Command("umount", mountpoint).CombinedOutput()
	}

	if err != nil {
		return fmt.Errorf("umount %s: %w\n%s", mountpoint, err, out)
	}
	fmt.Printf("Unmounted %s\n", mountpoint)
	return nil
}
