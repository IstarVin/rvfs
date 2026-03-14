package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
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
		absMountpoint, err := filepath.Abs(mountpoint)
		if err != nil {
			return fmt.Errorf("resolve mountpoint: %w", err)
		}

		isMP, err := isMountpoint(absMountpoint)
		if err != nil {
			return fmt.Errorf("check mountpoint %q: %w", absMountpoint, err)
		}
		if !isMP {
			return fmt.Errorf("%q is not a mountpoint", absMountpoint)
		}

		reg, err := ipc.OpenMountRegistry()
		if err != nil {
			return fmt.Errorf("open mount registry: %w", err)
		}
		defer reg.Close()

		entry, alive, err := reg.Lookup(absMountpoint)
		if err != nil {
			return fmt.Errorf("lookup mountpoint in registry: %w", err)
		}
		if !alive {
			return fmt.Errorf("%q is mounted but not managed by rvfs", absMountpoint)
		}

		summary := drainPending(cmd, entry)
		return unmount(cmd, absMountpoint, summary)
	},
}

// isMountpoint returns whether path is a mountpoint by comparing inode/device
// metadata against its parent directory.
func isMountpoint(path string) (bool, error) {
	cleanPath := filepath.Clean(path)

	st, err := os.Stat(cleanPath)
	if err != nil {
		return false, err
	}
	parentPath := filepath.Dir(cleanPath)
	parentSt, err := os.Stat(parentPath)
	if err != nil {
		return false, err
	}

	cur, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("unexpected stat type for %q", cleanPath)
	}
	parent, ok := parentSt.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("unexpected stat type for %q", parentPath)
	}

	if cur.Dev != parent.Dev {
		return true, nil
	}
	if cur.Ino == parent.Ino {
		return true, nil
	}
	return false, nil
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
