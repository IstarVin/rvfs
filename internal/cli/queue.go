package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/spf13/cobra"
)

var queueJSON bool

type queueEntry struct {
	Priority  int    `json:"priority"`
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Detail    string `json:"detail,omitempty"`
	Size      int64  `json:"size_bytes,omitempty"`
	QueuedAt  int64  `json:"queued_at,omitempty"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
}

type queueView struct {
	Source     string       `json:"source"`
	Total      int          `json:"total"`
	Operations []queueEntry `json:"operations"`
}

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
			fprintln(cmd.OutOrStdout(), "No pending operations.")
			return nil
		}

		view, uploadBytes, retries := buildQueueView(args[0], cl.DB(), ops)
		if queueJSON {
			return writeJSON(cmd.OutOrStdout(), view)
		}

		counts := make(map[string]int)
		for _, op := range view.Operations {
			counts[op.Operation]++
		}
		opsSummary := make([]string, 0, len(counts))
		for op, count := range counts {
			opsSummary = append(opsSummary, fmt.Sprintf("%d %s", count, op))
		}
		sort.Strings(opsSummary)
		printSection(cmd.OutOrStdout(), fmt.Sprintf("Queue for %s", args[0]))
		fprintf(cmd.OutOrStdout(), "%d pending %s", len(view.Operations), pluralize(len(view.Operations), "operation", "operations"))
		if len(opsSummary) > 0 {
			fprintf(cmd.OutOrStdout(), " (%s)", strings.Join(opsSummary, ", "))
		}
		if uploadBytes > 0 {
			fprintf(cmd.OutOrStdout(), ", %s queued to upload", humanBytes(uploadBytes))
		}
		if oldestQueuedAt := oldestQueuedTimestamp(view.Operations); oldestQueuedAt > 0 {
			age := time.Since(time.Unix(oldestQueuedAt, 0))
			fprintf(cmd.OutOrStdout(), ", oldest item waiting %s", humanDurationLong(age))
		}
		fprintf(cmd.OutOrStdout(), "\n")

		rows := make([][]string, 0, len(view.Operations))
		for _, op := range view.Operations {
			status := "queued"
			if op.Attempts > 0 {
				status = fmt.Sprintf("attempt %d", op.Attempts+1)
			}
			if op.LastError != "" {
				status = warnStyle.Render("retrying")
			}
			size := "-"
			if op.Size > 0 {
				size = humanBytes(op.Size)
			}
			detail := op.Detail
			if op.LastError != "" {
				detail = ellipsize(op.LastError, 28)
			}
			if detail == "" {
				detail = "-"
			}
			rows = append(rows, []string{
				fmt.Sprintf("%d", op.Priority),
				op.Operation,
				op.Path,
				detail,
				size,
				formatRelativeTime(op.QueuedAt),
				status,
			})
		}
		renderTable(cmd.OutOrStdout(), []tableColumn{
			{Title: "#", Width: 4, AlignRight: true},
			{Title: "OP", Width: 8},
			{Title: "PATH", Width: 34},
			{Title: "DETAIL", Width: 28},
			{Title: "SIZE", Width: 10, AlignRight: true},
			{Title: "WAITING", Width: 10},
			{Title: "STATUS", Width: 10},
		}, rows)
		if retries > 0 {
			printHint(cmd.OutOrStdout(), "run 'rvfs sync %s --force' to clear retry timers for failed operations", args[0])
		}
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

func buildQueueView(source string, db *cache.MetadataDB, ops []*cache.PendingOp) (queueView, int64, int) {
	view := queueView{Source: source, Total: len(ops), Operations: make([]queueEntry, 0, len(ops))}
	var uploadBytes int64
	retries := 0
	for i, op := range ops {
		entry := queueEntry{
			Priority:  i + 1,
			Operation: op.Op,
			Path:      op.Path,
			Detail:    queueDetail(op),
			QueuedAt:  op.QueuedAt,
			Attempts:  op.Attempts,
			LastError: op.LastError,
		}
		if op.Op == "put" {
			if file, err := db.GetFile(op.Path); err == nil && file != nil {
				entry.Size = file.Size
				uploadBytes += file.Size
			}
		}
		if op.LastError != "" || op.Attempts > 0 {
			retries++
		}
		view.Operations = append(view.Operations, entry)
	}
	return view, uploadBytes, retries
}

func queueDetail(op *cache.PendingOp) string {
	if op.DestPath != "" {
		return "-> " + op.DestPath
	}
	if op.LastError != "" {
		return op.LastError
	}
	return ""
}

func oldestQueuedTimestamp(ops []queueEntry) int64 {
	oldest := int64(0)
	for _, op := range ops {
		if op.QueuedAt <= 0 {
			continue
		}
		if oldest == 0 || op.QueuedAt < oldest {
			oldest = op.QueuedAt
		}
	}
	return oldest
}

func init() {
	queueCmd.Flags().BoolVar(&queueJSON, "json", false, "Output machine-readable JSON")
}
