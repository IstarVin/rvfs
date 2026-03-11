package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/IstarVin/rvfs/internal/config"
	"github.com/IstarVin/rvfs/internal/remote/gdrive"
	"github.com/spf13/cobra"
)

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage remote storage backends",
}

var remoteAddCmd = &cobra.Command{
	Use:   "add <type> <name>",
	Short: "Add a new remote",
	Long:  "Add a new remote storage backend. Supported types: gdrive",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteType := args[0]
		remoteName := args[1]

		if remoteType != "gdrive" {
			return fmt.Errorf("unsupported remote type %q (supported: gdrive)", remoteType)
		}

		cfgPath := config.DefaultPath()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		if _, exists := cfg.Remotes[remoteName]; exists {
			return fmt.Errorf("remote %q already exists", remoteName)
		}

		reader := bufio.NewReader(os.Stdin)

		fmt.Print("Client ID: ")
		clientID, _ := reader.ReadString('\n')
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			return fmt.Errorf("client ID is required")
		}

		fmt.Print("Client Secret: ")
		clientSecret, _ := reader.ReadString('\n')
		clientSecret = strings.TrimSpace(clientSecret)
		if clientSecret == "" {
			return fmt.Errorf("client secret is required")
		}

		fmt.Print("Root path (leave empty for entire Drive): ")
		rootPath, _ := reader.ReadString('\n')
		rootPath = strings.TrimSpace(rootPath)

		// Save config before auth (so if auth fails, user doesn't have to
		// re-enter credentials).
		cfg.Remotes[remoteName] = config.RemoteConfig{
			Type:         remoteType,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RootPath:     rootPath,
		}
		if err := cfg.Save(cfgPath); err != nil {
			return fmt.Errorf("save config: %w", err)
		}

		// Run OAuth2 flow.
		tokenPath := config.TokenPath(remoteName)
		fmt.Println()
		if err := gdrive.Authenticate(clientID, clientSecret, tokenPath); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		fmt.Fprintf(os.Stderr, "\nRemote %q added successfully.\n", remoteName)
		return nil
	},
}

var remoteAuthCmd = &cobra.Command{
	Use:   "auth <name>",
	Short: "Re-authenticate a remote",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteName := args[0]

		cfgPath := config.DefaultPath()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		rc, exists := cfg.Remotes[remoteName]
		if !exists {
			return fmt.Errorf("remote %q not found", remoteName)
		}

		tokenPath := config.TokenPath(remoteName)
		if err := gdrive.Authenticate(rc.ClientID, rc.ClientSecret, tokenPath); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		fmt.Fprintf(os.Stderr, "Remote %q re-authenticated.\n", remoteName)
		return nil
	},
}

var remoteListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured remotes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(config.DefaultPath())
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		if len(cfg.Remotes) == 0 {
			fmt.Println("No remotes configured. Use 'rvfs remote add' to add one.")
			return nil
		}

		for name, rc := range cfg.Remotes {
			root := rc.RootPath
			if root == "" {
				root = "/"
			}
			fmt.Printf("%-20s type=%-8s root=%s\n", name, rc.Type, root)
		}
		return nil
	},
}

var remoteRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a configured remote",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteName := args[0]

		cfgPath := config.DefaultPath()
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		if _, exists := cfg.Remotes[remoteName]; !exists {
			return fmt.Errorf("remote %q not found", remoteName)
		}

		delete(cfg.Remotes, remoteName)
		if err := cfg.Save(cfgPath); err != nil {
			return fmt.Errorf("save config: %w", err)
		}

		// Remove token file if it exists.
		tokenPath := config.TokenPath(remoteName)
		os.Remove(tokenPath)

		fmt.Fprintf(os.Stderr, "Remote %q removed.\n", remoteName)
		return nil
	},
}

func init() {
	remoteCmd.AddCommand(remoteAddCmd, remoteAuthCmd, remoteListCmd, remoteRemoveCmd)
}
