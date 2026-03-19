package cmd

import (
	"context"
	"fmt"
	"os"
	sig "os/signal"
	"syscall"

	"github.com/julienrbrt/talktothem/internal/agent"
	"github.com/julienrbrt/talktothem/internal/config"
	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/llm"
	"github.com/julienrbrt/talktothem/internal/messenger"
	"github.com/julienrbrt/talktothem/internal/messenger/signal"
	"github.com/spf13/cobra"
)

var cfgFile string

func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "talktothem",
		Short: "AI agent that talks to your friends and family for you",
		Long: `TalkToThem learns your conversation style by analyzing your message history,
then can hold conversations on your behalf with your contacts.`,
	}

	cmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/talktothem/config.yaml)")

	cmd.AddCommand(newStartCommand())
	cmd.AddCommand(newContactsCommand())
	cmd.AddCommand(newLearnCommand())
	cmd.AddCommand(newConfigCommand())

	return cmd
}

func newStartCommand() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the agent",
		Long:  "Start the agent to begin responding to messages on your behalf",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			llmClient, err := createLLMClient(cfg)
			if err != nil {
				return err
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("failed to create contact manager: %w", err)
			}

			a := agent.New(llmClient, llmClient, contacts, cfg.Contact.DataPath)

			signalClient := signal.New(cfg.Signal.PhoneNumber,
				signal.WithDataPath(cfg.Signal.DataPath),
			)

			if dryRun {
				fmt.Println("Dry run mode - not connecting to Signal")
				return nil
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			a.OnMessage(func(msg messenger.Message) {
				fmt.Printf("[%s] Sending: %s\n", msg.ContactID, msg.Content)
				if !dryRun {
					if err := signalClient.SendMessage(ctx, msg.ContactID, msg.Content); err != nil {
						fmt.Fprintf(os.Stderr, "Failed to send message: %v\n", err)
					}
				}
			})

			fmt.Println("Starting TalkToThem agent...")
			fmt.Println("Press Ctrl+C to stop")

			if err := a.Start(ctx, signalClient); err != nil {
				return fmt.Errorf("failed to start agent: %w", err)
			}

			sigChan := make(chan os.Signal, 1)
			sig.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			<-sigChan

			fmt.Println("\nShutting down...")
			return signalClient.Disconnect()
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Run without sending messages")

	return cmd
}

func newContactsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "contacts",
		Short: "Manage contacts",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List all contacts",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("failed to create contact manager: %w", err)
			}

			for _, c := range contacts.List() {
				status := "disabled"
				if c.Enabled {
					status = "enabled"
				}
				fmt.Printf("%s (%s) - %s\n", c.Name, c.Phone, status)
			}

			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "enable <phone>",
		Short: "Enable a contact for auto-response",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("failed to create contact manager: %w", err)
			}

			if err := contacts.SetEnabled(args[0], true); err != nil {
				return err
			}

			fmt.Printf("Enabled contact: %s\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "disable <phone>",
		Short: "Disable a contact for auto-response",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("failed to create contact manager: %w", err)
			}

			if err := contacts.SetEnabled(args[0], false); err != nil {
				return err
			}

			fmt.Printf("Disabled contact: %s\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "add <phone> <name>",
		Short: "Add a new contact",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("failed to create contact manager: %w", err)
			}

			c := contact.Contact{
				ID:      args[0],
				Phone:   args[0],
				Name:    args[1],
				Enabled: false,
			}

			if err := contacts.Add(c); err != nil {
				return err
			}

			fmt.Printf("Added contact: %s (%s)\n", args[1], args[0])
			return nil
		},
	})

	return cmd
}

func newLearnCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "learn <contact-phone>",
		Short: "Learn your conversation style with a contact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			llmClient, err := createLLMClient(cfg)
			if err != nil {
				return err
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("failed to create contact manager: %w", err)
			}

			a := agent.New(llmClient, llmClient, contacts, cfg.Contact.DataPath)

			style, err := a.LearnStyle(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to learn style: %w", err)
			}

			fmt.Printf("Learned style: %s\n", style)
			return nil
		},
	}
}

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Create a sample configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.DefaultConfigPath()
			if err != nil {
				return err
			}

			sample := `# TalkToThem Configuration
# https://github.com/julienrbrt/talktothem

signal:
  phone_number: "+1234567890"
  data_path: ""

agent:
  api_key: "sk-..."  # required
  model: "gpt-4o"
  base_url: ""  # optional, for OpenAI-compatible APIs

contact:
  data_path: ""
`

			fmt.Printf("Sample configuration:\n\n%s\n", sample)
			fmt.Printf("Save this to: %s\n", path)
			return nil
		},
	})

	return cmd
}

func createLLMClient(cfg *config.Config) (*llm.Client, error) {
	if cfg.Agent.APIKey == "" {
		return nil, fmt.Errorf("agent.api_key is required in config")
	}

	model := cfg.Agent.Model
	if model == "" {
		model = "gpt-4o"
	}

	return llm.NewClient(llm.Config{
		APIKey:  cfg.Agent.APIKey,
		BaseURL: cfg.Agent.BaseURL,
		Model:   model,
	}), nil
}
