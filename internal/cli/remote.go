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

var remoteListJSON bool

type remoteListEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Root string `json:"root"`
}

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

		reader := bufio.NewReader(cmd.InOrStdin())

		fprintf(cmd.OutOrStdout(), "Client ID: ")
		clientID, _ := reader.ReadString('\n')
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			return fmt.Errorf("client ID is required")
		}

		fprintf(cmd.OutOrStdout(), "Client Secret: ")
		clientSecret, _ := reader.ReadString('\n')
		clientSecret = strings.TrimSpace(clientSecret)
		if clientSecret == "" {
			return fmt.Errorf("client secret is required")
		}

		fprintf(cmd.OutOrStdout(), "Root path (leave empty for entire Drive): ")
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
		fprintln(cmd.OutOrStdout())
		printSection(cmd.OutOrStdout(), fmt.Sprintf("Authorizing %s", remoteName))
		printHint(cmd.OutOrStdout(), "complete the browser-based Google Drive flow and return here")
		if err := gdrive.Authenticate(clientID, clientSecret, tokenPath); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}

		printSuccess(cmd.OutOrStdout(), "remote %q added", remoteName)
		printKeyValues(cmd.OutOrStdout(), [][2]string{{"Type:", remoteType}, {"Root:", defaultRemoteRoot(rootPath)}, {"Token:", tokenPath}})
		printHint(cmd.OutOrStdout(), "mount it with 'rvfs mount %s: <mountpoint>'", remoteName)
		fprintln(cmd.OutOrStdout())
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

		printSuccess(cmd.OutOrStdout(), "remote %q re-authenticated", remoteName)
		printKeyValues(cmd.OutOrStdout(), [][2]string{{"Token:", tokenPath}})
		fprintln(cmd.OutOrStdout())
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
			fprintln(cmd.OutOrStdout(), "No remotes configured. Use 'rvfs remote add' to add one.")
			return nil
		}

		remoteNames := make(map[string]struct{}, len(cfg.Remotes))
		for name := range cfg.Remotes {
			remoteNames[name] = struct{}{}
		}
		names := sortedRemoteNames(remoteNames)

		rows := make([][]string, 0, len(names))
		items := make([]remoteListEntry, 0, len(names))
		for _, name := range names {
			rc := cfg.Remotes[name]
			root := rc.RootPath
			if root == "" {
				root = "/"
			}
			items = append(items, remoteListEntry{Name: name, Type: rc.Type, Root: root})
			rows = append(rows, []string{name, rc.Type, root})
		}
		if remoteListJSON {
			return writeJSON(cmd.OutOrStdout(), items)
		}
		printSection(cmd.OutOrStdout(), "Configured remotes")
		renderTable(cmd.OutOrStdout(), []tableColumn{{Title: "NAME", Width: 22}, {Title: "TYPE", Width: 10}, {Title: "ROOT", Width: 36}}, rows)
		fprintln(cmd.OutOrStdout())
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

		printSuccess(cmd.OutOrStdout(), "remote %q removed", remoteName)
		printKeyValues(cmd.OutOrStdout(), [][2]string{{"Token removed:", tokenPath}})
		fprintln(cmd.OutOrStdout())
		return nil
	},
}

func defaultRemoteRoot(root string) string {
	if root == "" {
		return "/"
	}
	return root
}

func init() {
	remoteListCmd.Flags().BoolVar(&remoteListJSON, "json", false, "Output machine-readable JSON")
	remoteCmd.AddCommand(remoteAddCmd, remoteAuthCmd, remoteListCmd, remoteRemoveCmd)
}
