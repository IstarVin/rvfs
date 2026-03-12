package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
    "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.rvfs.{{.Name}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.ExecPath}}</string>
        <string>mount</string>
        <string>{{.Source}}</string>
        <string>{{.Mountpoint}}</string>
        <string>--foreground</string>{{range .ExtraArgs}}
        <string>{{.}}</string>{{end}}
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
</dict>
</plist>
`

type launchdPlist struct {
	Name       string
	ExecPath   string
	Source     string
	Mountpoint string
	ExtraArgs  []string
	LogPath    string
}

// agentDir returns the LaunchAgents directory, creating it if necessary.
func agentDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	return dir, nil
}

// plistPath returns the full path for the named plist file.
func plistPath(name string) (string, error) {
	dir, err := agentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "com.rvfs."+name+".plist"), nil
}

// InstallLaunchdService writes a launchd plist that runs
// `rvfs mount <source> <mountpoint> --foreground [extraArgs...]` and loads it.
func InstallLaunchdService(name, source, mountpoint string, extraArgs []string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}

	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, "Library", "Logs", "rvfs-"+name+".log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0755)

	// Strip --foreground from extraArgs: we always add it explicitly in the template.
	filtered := make([]string, 0, len(extraArgs))
	for _, a := range extraArgs {
		if !strings.EqualFold(a, "--foreground") {
			filtered = append(filtered, a)
		}
	}

	tmpl, err := template.New("plist").Parse(launchdTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	plist, err := plistPath(name)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(plist, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, launchdPlist{
		Name:       name,
		ExecPath:   execPath,
		Source:     source,
		Mountpoint: mountpoint,
		ExtraArgs:  filtered,
		LogPath:    logPath,
	}); err != nil {
		return fmt.Errorf("generate plist: %w", err)
	}

	// Load the agent immediately.
	if out, err := exec.Command("launchctl", "load", "-w", plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, out)
	}

	fmt.Printf("Installed launchd agent: %s\n", plist)
	return nil
}

// UninstallLaunchdService unloads and removes the launchd agent for name.
func UninstallLaunchdService(name string) error {
	plist, err := plistPath(name)
	if err != nil {
		return err
	}
	// Best-effort unload.
	_ = exec.Command("launchctl", "unload", "-w", plist).Run()

	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Printf("Removed launchd agent: %s\n", plist)
	return nil
}
