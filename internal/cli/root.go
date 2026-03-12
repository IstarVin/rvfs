package cli

import (
	"fmt"

	"github.com/IstarVin/rvfs/internal/config"
	"github.com/spf13/cobra"
)

// globalCfg holds the loaded application config. It is populated by
// PersistentPreRunE before any sub-command runs. On error it falls back to
// defaults so the CLI remains usable even when no config file exists.
var globalCfg *config.Config

var rootCmd = &cobra.Command{
	Use:   "rvfs",
	Short: "An offline-first FUSE filesystem",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(config.DefaultPath())
		if err != nil {
			// Non-fatal: warn and continue with defaults.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not load config (%v); using defaults\n", err)
			cfg = &config.Config{
				Mount:   config.DefaultMountConfig(),
				Log:     config.DefaultLogConfig(),
				Remotes: make(map[string]config.RemoteConfig),
			}
		}
		globalCfg = cfg
		return nil
	},
}

// Execute runs the root command. Called from main.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(mountCmd)
	rootCmd.AddCommand(remoteCmd)
	rootCmd.AddCommand(conflictsCmd)
	rootCmd.AddCommand(resolveCmd)
}
