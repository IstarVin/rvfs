package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/IstarVin/rvfs/internal/cache"
	"github.com/IstarVin/rvfs/internal/ipc"
	"github.com/charmbracelet/lipgloss"
)

var (
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	labelStyle   = lipgloss.NewStyle().Bold(true).Width(12)
	valueStyle   = lipgloss.NewStyle()
	headerStyle  = lipgloss.NewStyle().Bold(true).Underline(true)
	mutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

type tableColumn struct {
	Title      string
	Width      int
	AlignRight bool
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printSection(w io.Writer, title string) {
	if title == "" {
		return
	}
	fprintln(w, sectionStyle.Render(title))
}

func printKeyValues(w io.Writer, rows [][2]string) {
	for _, row := range rows {
		fprintf(w, "%s %s\n", labelStyle.Render(row[0]), valueStyle.Render(row[1]))
	}
}

func printHint(w io.Writer, format string, args ...any) {
	fprintf(w, "%s %s\n", mutedStyle.Render("Hint:"), fmt.Sprintf(format, args...))
}

func printWarning(w io.Writer, format string, args ...any) {
	fprintf(w, "%s %s\n", warnStyle.Render("Warning:"), fmt.Sprintf(format, args...))
}

func printSuccess(w io.Writer, format string, args ...any) {
	fprintf(w, "%s %s\n", okStyle.Render("OK:"), fmt.Sprintf(format, args...))
}

func renderTable(w io.Writer, columns []tableColumn, rows [][]string) {
	if len(columns) == 0 {
		return
	}

	headerCells := make([]string, 0, len(columns))
	for _, column := range columns {
		headerCells = append(headerCells, headerStyle.Render(padCell(column.Title, column.Width, column.AlignRight)))
	}
	fprintln(w, strings.Join(headerCells, "  "))

	for _, row := range rows {
		cells := make([]string, 0, len(columns))
		for i, column := range columns {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			cells = append(cells, padCell(value, column.Width, column.AlignRight))
		}
		fprintln(w, strings.Join(cells, "  "))
	}
}

func padCell(value string, width int, alignRight bool) string {
	value = strings.ReplaceAll(value, "\n", " ")
	if width <= 0 {
		return value
	}
	value = ellipsize(value, width)
	if alignRight {
		return fmt.Sprintf("%*s", width, value)
	}
	return fmt.Sprintf("%-*s", width, value)
}

func ellipsize(value string, width int) string {
	if width <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func humanBytes(b int64) string {
	const (
		_  = iota
		KB = 1 << (10 * iota)
		MB
		GB
		TB
	)
	switch {
	case b >= TB:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(TB))
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func humanDurationLong(d time.Duration) string {
	if d < 0 {
		d = 0
	}
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

func humanDurationShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func formatRelativeTime(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return humanDurationShort(time.Since(time.Unix(ts, 0))) + " ago"
}

func formatTimestamp(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func statusBadge(ok bool, okText, warnText string) string {
	if ok {
		return okStyle.Render(okText)
	}
	return warnStyle.Render(warnText)
}

func sortedRemoteNames(remotes map[string]struct{}) []string {
	names := make([]string, 0, len(remotes))
	for name := range remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func fprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func fprintln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

// resolveSource parses a source argument (e.g. "gdrive:Documents" or
// "myremote:") and returns the remoteID, the ipc socket path and an open
// CacheLayer. The caller must Close the CacheLayer when done.
//
// The socket path is resolved via the mount registry when possible. If the
// registry shows multiple active mounts for the same source an error is
// returned; callers should direct the user to `rvfs status` for disambiguation.
func resolveSource(source string) (remoteID string, sockPath string, cl *cache.CacheLayer, err error) {
	before, _, ok := strings.Cut(source, ":")
	if !ok {
		err = fmt.Errorf("invalid source %q: expected <remote>:<path>", source)
		return
	}
	remoteID = before

	dir := getCacheDir()
	cl, err = cache.NewCacheLayer(dir, remoteID)
	if err != nil {
		err = fmt.Errorf("open cache for %q: %w", remoteID, err)
		return
	}

	// Try to get the exact socket path from the mount registry.
	reg, regErr := ipc.OpenMountRegistry()
	if regErr == nil {
		defer reg.Close()
		entries, listErr := reg.ListBySource(source)
		if listErr == nil {
			switch len(entries) {
			case 0:
				// Mount not running; leave sockPath empty so callers handle it.
			case 1:
				sockPath = entries[0].SockPath
				return
			default:
				err = fmt.Errorf(
					"multiple active mounts for %q; use 'rvfs status' to list them",
					source,
				)
				cl.Close()
				return
			}
		}
	}

	// Fallback: use the legacy per-remote socket path.
	sockPath = ipc.SockPath(remoteID)
	return
}

// resolveMountPath takes any absolute-or-relative filesystem path that lives
// inside a running FUSE mountpoint and returns the remoteID, the IPC socket
// path, an open CacheLayer, and the path relative to the mountpoint root.
// The caller must Close the CacheLayer when done.
func resolveMountPath(path string) (remoteID string, sockPath string, cl *cache.CacheLayer, rel string, err error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return
	}

	reg, err := ipc.OpenMountRegistry()
	if err != nil {
		err = fmt.Errorf("open mount registry: %w", err)
		return
	}
	defer reg.Close()

	entries, err := reg.ListAll()
	if err != nil {
		err = fmt.Errorf("list mounts: %w", err)
		return
	}

	// Pick the entry with the longest matching mountpoint prefix so that
	// nested mounts resolve to the innermost one.
	best := -1
	for i, e := range entries {
		mp := filepath.Clean(e.Mountpoint)
		if absPath == mp || strings.HasPrefix(absPath, mp+"/") {
			if best == -1 || len(mp) > len(filepath.Clean(entries[best].Mountpoint)) {
				best = i
			}
		}
	}
	if best == -1 {
		err = fmt.Errorf("%q is not inside any active rvfs mountpoint", path)
		return
	}

	e := entries[best]
	remoteID = e.RemoteName
	sockPath = e.SockPath
	rel = strings.TrimPrefix(absPath, filepath.Clean(e.Mountpoint))
	rel = strings.TrimPrefix(rel, "/")

	cl, err = cache.NewCacheLayer(getCacheDir(), remoteID)
	if err != nil {
		err = fmt.Errorf("open cache for %q: %w", remoteID, err)
	}
	return
}

// getCacheDir returns the effective cache directory, honouring the global
// config when available.
func getCacheDir() string {
	if globalCfg != nil && globalCfg.Mount.CacheDir != "" {
		return globalCfg.Mount.CacheDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", "rvfs")
	}
	return filepath.Join(home, ".cache", "rvfs")
}
