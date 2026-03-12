package cli

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
	Use:   "rvfs",
	Short: "An offline-first FUSE filesystem",
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
