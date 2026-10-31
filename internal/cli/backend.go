package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"remote.network/node/internal/backends"
)

// newBackendCommand is the registry surface: the dossier
// provider pattern — `dossierctl provider add --kind --base-url`, plural.
// `node start` reads this registry unless single-backend flags override.
func newBackendCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backend",
		Short: "Manage the agent's backend registry (~/.remote/backends.json)",
	}
	cmd.AddCommand(newBackendAddCommand(), newBackendListCommand(), newBackendRemoveCommand())
	return cmd
}

func newBackendAddCommand() *cobra.Command {
	var (
		id     string
		kind   string
		url    string
		apiKey string
		models []string
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add one inference endpoint to the registry",
		Example: `  remote backend add --id ollama-lan --kind openai-compatible --url http://ollama-box.lan:11434/v1
  remote backend add --id vllm-2 --kind openai --url https://vllm.internal:8000/v1 --api-key sk-... --model qwen3-32b`,
		RunE: func(cmd *cobra.Command, args []string) error {
			e := backends.Entry{ID: id, Kind: kind, URL: url, APIKey: apiKey, Models: models}
			if apiKey == "" {
				e.APIKey = osKeyEnv() // env fallback, same rule as node start
			}
			if err := backends.Add(homeDir, e); err != nil {
				return err
			}
			fmt.Printf("backend %q added (%s) — 'remote backend list' to review\n", id, kind)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "registry id (e.g. ollama-lan, vllm-2)")
	cmd.Flags().StringVar(&kind, "kind", "openai-compatible",
		"wire kind: openai | openai-compatible | anthropic")
	cmd.Flags().StringVar(&url, "url", "",
		"base URL (default for openai-compatible: http://localhost:11434/v1)")
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"API key (or env REMOTE_BACKEND_API_KEY; loopback needs none)")
	cmd.Flags().StringSliceVar(&models, "model", nil,
		"restrict this backend to these raw ids (repeatable)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func newBackendListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print the registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := backends.Load(homeDir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				url := e.URL
				if url == "" {
					url = "(default: http://localhost:11434/v1)"
				}
				fmt.Printf("%-16s %-18s %s", e.ID, e.Kind, url)
				if len(e.Models) > 0 {
					fmt.Printf("  models: %s", strings.Join(e.Models, ","))
				}
				if e.APIKey != "" {
					fmt.Print("  [keyed]")
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func newBackendRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove one entry from the registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := backends.Remove(homeDir, args[0]); err != nil {
				return err
			}
			fmt.Printf("backend %q removed\n", args[0])
			return nil
		},
	}
}

// osKeyEnv mirrors backendFlags.key()'s env fallback.
func osKeyEnv() string { return envOr("REMOTE_BACKEND_API_KEY", "") }
