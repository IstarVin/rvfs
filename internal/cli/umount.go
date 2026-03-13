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

type drainResult struct {
	Waited         bool
	Aborted        bool
	InitialPending int
	FinalPending   int
	Source         string
}

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
		summary := drainResult{}

		reg, regErr := ipc.OpenMountRegistry()
		if regErr == nil {
			defer reg.Close()
			entry, alive, _ := reg.Lookup(absMountpoint)
			if alive {
				summary = drainPending(cmd, entry)
			} else {
				printWarning(cmd.ErrOrStderr(), "check 'rvfs queue' first to ensure all uploads are complete")
			}
		} else {
			printWarning(cmd.ErrOrStderr(), "check 'rvfs queue' first to ensure all uploads are complete")
		}

		return unmount(cmd, mountpoint, summary)
	},
}

// drainPending connects to the running mount, shows pending op count, and
// blocks until all uploads finish or the user presses Ctrl-C.
func drainPending(cmd *cobra.Command, entry ipc.MountEntry) drainResult {
	result := drainResult{Source: entry.Source}
	c, err := ipc.Dial(entry.SockPath)
	if err != nil {
		return result
	}
	defer c.Close()

	resp, err := c.Status()
	if err != nil || resp.Pending == 0 {
		return result
	}
	result.Waited = true
	result.InitialPending = resp.Pending

	fprintf(cmd.OutOrStdout(),
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
				result.Aborted = true
				result.FinalPending = resp2.Pending
				fprintf(cmd.OutOrStdout(),
					"\nAborted. Unmounting with %d pending op(s) remaining.\n", resp2.Pending)
			} else {
				result.Aborted = true
				fprintln(cmd.OutOrStdout(), "\nAborted.")
			}
			return result
		case <-ticker.C:
			resp2, err2 := c.Status()
			if err2 != nil {
				return result
			}
			fprintf(cmd.OutOrStdout(), "\r%d pending upload(s) remaining...  ", resp2.Pending)
			if resp2.Pending == 0 {
				result.FinalPending = 0
				fprintln(cmd.OutOrStdout(), "\nAll uploads complete.")
				return result
			}
			resp = resp2
			result.FinalPending = resp2.Pending
		}
	}
}

func unmount(cmd *cobra.Command, mountpoint string, summary drainResult) error {
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
	printSuccess(cmd.OutOrStdout(), "unmounted %s", mountpoint)
	if summary.Waited {
		state := fmt.Sprintf("started with %d pending upload(s)", summary.InitialPending)
		if summary.Aborted {
			state = fmt.Sprintf("aborted with %d pending upload(s) remaining", summary.FinalPending)
		} else {
			state = "uploads drained before unmount"
		}
		printKeyValues(cmd.OutOrStdout(), [][2]string{{"Drain:", state}})
	}
	fprintln(cmd.OutOrStdout())
	return nil
}
