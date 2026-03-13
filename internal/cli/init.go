package cli

import (
	"fmt"
	"os"

	"github.com/IstarVin/rvfs/internal/config"
	"github.com/spf13/cobra"
)

var initForce bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialise rvfs with a default configuration file",
	Long: `Create the rvfs configuration directory and write a default config.toml.

The file is written to the platform config directory (e.g. ~/.config/rvfs/config.toml).
If the file already exists the command exits without modifying it unless --force is given.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgPath := config.DefaultPath()

		if !initForce {
			if _, err := os.Stat(cfgPath); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Config already exists at %s (use --force to overwrite)\n", cfgPath)
				return nil
			}
		}

		cfg := &config.Config{
			Mount:   config.DefaultMountConfig(),
			Log:     config.DefaultLogConfig(),
			Remotes: make(map[string]config.RemoteConfig),
		}

		if err := cfg.Save(cfgPath); err != nil {
			return fmt.Errorf("write config: %w", err)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Initialised rvfs config at %s\n", cfgPath)
		return nil
	},
}

func init() {
	initCmd.Flags().BoolVarP(&initForce, "force", "f", false, "Overwrite existing config file")
}
