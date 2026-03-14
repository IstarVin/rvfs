package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/spf13/cobra"
)

var queueJSON bool

type queueEntry struct {
	Priority        int     `json:"priority"`
	Operation       string  `json:"operation"`
	Path            string  `json:"path"`
	Detail          string  `json:"detail,omitempty"`
	Size            int64   `json:"size_bytes,omitempty"`
	QueuedAt        int64   `json:"queued_at,omitempty"`
	Attempts        int     `json:"attempts"`
	LastError       string  `json:"last_error,omitempty"`
	Status          string  `json:"status,omitempty"`
	UploadedBytes   int64   `json:"uploaded_bytes,omitempty"`
	ProgressPercent float64 `json:"progress_percent,omitempty"`
	StartedAt       int64   `json:"started_at,omitempty"`
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
		_, sockPath, cl, err := resolveSource(args[0])
		if err != nil {
			return err
		}
		defer cl.Close()

		ops, err := cl.DB().NextPendingOps(1000)
		if err != nil {
			return fmt.Errorf("list pending ops: %w", err)
		}
		uploads := loadActiveUploads(sockPath)
		if len(ops) == 0 && len(uploads) == 0 {
			fprintln(cmd.OutOrStdout(), "No pending operations.")
			return nil
		}

		view, uploadBytes, retries, activeUploads := buildQueueView(args[0], cl.DB(), ops, uploads)
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
		if activeUploads > 0 {
			fprintf(cmd.OutOrStdout(), ", %d %s uploading", activeUploads, pluralize(activeUploads, "item", "items"))
		}
		if oldestQueuedAt := oldestQueuedTimestamp(view.Operations); oldestQueuedAt > 0 {
			age := time.Since(time.Unix(oldestQueuedAt, 0))
			fprintf(cmd.OutOrStdout(), ", oldest item waiting %s", humanDurationLong(age))
		}
		fprintf(cmd.OutOrStdout(), "\n")

		rows := make([][]string, 0, len(view.Operations))
		for _, op := range view.Operations {
			status := queueStatusLabel(op)
			progress := queueProgressLabel(op)
			size := "-"
			if op.Size > 0 {
				size = humanBytes(op.Size)
			}
			detail := queueDetailLabel(op)
			if detail == "" {
				detail = "-"
			}
			rows = append(rows, []string{
				fmt.Sprintf("%d", op.Priority),
				op.Operation,
				op.Path,
				detail,
				size,
				progress,
				formatRelativeTime(op.QueuedAt),
				status,
			})
		}
		renderTable(cmd.OutOrStdout(), []tableColumn{
			{Title: "#", Width: 4, AlignRight: true},
			{Title: "OP", Width: 8},
			{Title: "PATH", Width: 30},
			{Title: "DETAIL", Width: 28},
			{Title: "SIZE", Width: 10, AlignRight: true},
			{Title: "PROGRESS", Width: 12},
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

func buildQueueView(source string, db *cache.MetadataDB, ops []*cache.PendingOp, uploads map[string]ipc.UploadStatusEntry) (queueView, int64, int, int) {
	view := queueView{Source: source, Operations: make([]queueEntry, 0, len(ops)+len(uploads))}
	var uploadBytes int64
	retries := 0
	activeUploads := 0
	remainingUploads := make(map[string]ipc.UploadStatusEntry, len(uploads))
	for path, upload := range uploads {
		remainingUploads[path] = upload
	}
	for i, op := range ops {
		entry := queueEntry{
			Priority:  i + 1,
			Operation: op.Op,
			Path:      op.Path,
			Detail:    queueDetail(op),
			QueuedAt:  op.QueuedAt,
			Attempts:  op.Attempts,
			LastError: op.LastError,
			Status:    queueStatus(op),
		}
		if op.Op == "put" {
			if file, err := db.GetFile(op.Path); err == nil && file != nil {
				entry.Size = file.Size
				uploadBytes += file.Size
			}
			if upload, ok := remainingUploads[op.Path]; ok {
				entry.Status = upload.State
				entry.UploadedBytes = upload.Uploaded
				entry.StartedAt = upload.StartedAt
				if upload.TotalSize > 0 {
					entry.Size = upload.TotalSize
				}
				entry.ProgressPercent = queueProgressPercent(upload.Uploaded, entry.Size)
				activeUploads++
				delete(remainingUploads, op.Path)
			}
		}
		if op.LastError != "" || op.Attempts > 0 {
			retries++
		}
		view.Operations = append(view.Operations, entry)
	}
	for _, upload := range remainingUploads {
		activeUploads++
		view.Operations = append(view.Operations, queueEntry{
			Priority:        len(view.Operations) + 1,
			Operation:       "put",
			Path:            upload.Path,
			Size:            upload.TotalSize,
			Status:          upload.State,
			UploadedBytes:   upload.Uploaded,
			ProgressPercent: queueProgressPercent(upload.Uploaded, upload.TotalSize),
			StartedAt:       upload.StartedAt,
		})
	}
	view.Total = len(view.Operations)
	return view, uploadBytes, retries, activeUploads
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

func queueStatus(op *cache.PendingOp) string {
	if op.LastError != "" {
		return "retrying"
	}
	if op.Attempts > 0 {
		return fmt.Sprintf("attempt %d", op.Attempts+1)
	}
	return "queued"
}

func queueStatusLabel(op queueEntry) string {
	if op.Status == "retrying" {
		return warnStyle.Render(op.Status)
	}
	if op.Status == "uploading" {
		return okStyle.Render(op.Status)
	}
	if op.Status != "" {
		return op.Status
	}
	return "queued"
}

func queueProgressLabel(op queueEntry) string {
	if op.Status != "uploading" {
		return "-"
	}
	if op.Size <= 0 {
		return humanBytes(op.UploadedBytes)
	}
	return fmt.Sprintf("%.0f%%", op.ProgressPercent)
}

func queueDetailLabel(op queueEntry) string {
	if op.LastError != "" {
		return ellipsize(op.LastError, 28)
	}
	if op.Status == "uploading" {
		return fmt.Sprintf("%s / %s", humanBytes(op.UploadedBytes), humanBytes(op.Size))
	}
	return op.Detail
}

func queueProgressPercent(uploaded, total int64) float64 {
	if total <= 0 {
		return 0
	}
	pct := float64(uploaded) / float64(total) * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func loadActiveUploads(sockPath string) map[string]ipc.UploadStatusEntry {
	uploads := make(map[string]ipc.UploadStatusEntry)
	if sockPath == "" {
		return uploads
	}
	c, err := ipc.Dial(sockPath)
	if err != nil {
		return uploads
	}
	defer c.Close()

	resp, err := c.Uploads("")
	if err != nil {
		return uploads
	}
	for _, entry := range resp.Entries {
		uploads[entry.Path] = entry
	}
	return uploads
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
