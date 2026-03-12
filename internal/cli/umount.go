package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var umountCmd = &cobra.Command{
	Use:   "umount <mountpoint>",
	Short: "Unmount a FUSE filesystem",
	Long: `Gracefully unmount an rvfs FUSE filesystem.

If there are pending uploads the command waits for them to drain before
unmounting. Press Ctrl-C to abort the wait and unmount immediately.

  rvfs umount ~/Documents`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mountpoint := args[0]
		absMountpoint, _ := filepath.Abs(mountpoint)

		reg, regErr := ipc.OpenMountRegistry()
		if regErr == nil {
			defer reg.Close()
			entry, alive, _ := reg.Lookup(absMountpoint)
			if alive {
				drainPending(cmd, entry)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Warning: check `rvfs queue` first to ensure all uploads are complete.\n")
			}
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Warning: check `rvfs queue` first to ensure all uploads are complete.\n")
		}

		return unmount(mountpoint)
	},
}

// drainPending connects to the running mount, shows pending op count, and
// blocks until all uploads finish or the user presses Ctrl-C.
func drainPending(cmd *cobra.Command, entry ipc.MountEntry) {
	c, err := ipc.Dial(entry.SockPath)
	if err != nil {
		return
	}
	defer c.Close()

	resp, err := c.Status()
	if err != nil || resp.Pending == 0 {
		return
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"%s has %d pending upload(s). Waiting for drain... (Ctrl-C to abort)\n",
		entry.Source, resp.Pending)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if resp2, err2 := c.Status(); err2 == nil {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nAborted. Unmounting with %d pending op(s) remaining.\n", resp2.Pending)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "\nAborted.")
			}
			return
		case <-ticker.C:
			resp2, err2 := c.Status()
			if err2 != nil {
				return
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\r%d pending upload(s) remaining...  ", resp2.Pending)
			if resp2.Pending == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "\nAll uploads complete.")
				return
			}
			resp = resp2
		}
	}
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
