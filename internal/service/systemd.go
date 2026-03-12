// Package service generates OS-level service units for auto-starting rvfs at
// login (Linux systemd user units; macOS launchd plists).
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

const systemdTemplate = `[Unit]
Description=rvfs mount {{.Name}}
After=network.target

[Service]
Type=simple
ExecStart={{.ExecPath}} mount {{.Source}} {{.Mountpoint}}{{.ExtraArgs}}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

type systemdUnit struct {
	Name       string
	ExecPath   string
	Source     string
	Mountpoint string
	ExtraArgs  string
}

// unitDir returns the systemd user unit directory, creating it if necessary.
func unitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create unit dir: %w", err)
	}
	return dir, nil
}

// unitPath returns the path of the service file for the given mount name.
func unitPath(name string) (string, error) {
	dir, err := unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rvfs-"+name+".service"), nil
}

// InstallSystemdService writes a systemd user unit that runs
// `rvfs mount <source> <mountpoint> --foreground [extraArgs...]` and enables
// it so it starts automatically on login.
func InstallSystemdService(name, source, mountpoint string, extraArgs []string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}

	extra := ""
	if len(extraArgs) > 0 {
		extra = " " + strings.Join(extraArgs, " ")
	}
	// Ensure --foreground is always present so the daemon sees the flag.
	if !strings.Contains(extra, "--foreground") {
		extra += " --foreground"
	}

	tmpl, err := template.New("unit").Parse(systemdTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	unitFile, err := unitPath(name)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(unitFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, systemdUnit{
		Name:       name,
		ExecPath:   execPath,
		Source:     source,
		Mountpoint: mountpoint,
		ExtraArgs:  extra,
	}); err != nil {
		return fmt.Errorf("generate unit: %w", err)
	}

	// Reload systemd and enable (and start) the unit.
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", "rvfs-" + name + ".service"},
	} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %v: %w\n%s", args, err, out)
		}
	}

	fmt.Printf("Installed systemd unit: %s\n", unitFile)
	return nil
}

// UninstallSystemdService stops and removes the systemd user unit for name.
func UninstallSystemdService(name string) error {
	unitName := "rvfs-" + name + ".service"
	for _, args := range [][]string{
		{"--user", "disable", "--now", unitName},
	} {
		// Best-effort: ignore errors (unit may already be inactive).
		_ = exec.Command("systemctl", args...).Run()
	}

	path, err := unitPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit file: %w", err)
	}

	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("Removed systemd unit: %s\n", path)
	return nil
}
