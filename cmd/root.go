package cmd

import (
	"context"
	"fmt"
	"os"
	sig "os/signal"
	"path/filepath"
	"syscall"

	"github.com/julienrbrt/talktothem/internal/agent"
	"github.com/julienrbrt/talktothem/internal/api"
	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/llm"
	"github.com/julienrbrt/talktothem/internal/messenger"
	signalcli "github.com/julienrbrt/talktothem/internal/messenger/signal"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "talktothem",
		Short: "AI agent that talks to your friends and family for you",
		Long: `TalkToThem learns your conversation style by analyzing your message history,
then can hold conversations on your behalf with your contacts.

Run without arguments to start the web UI server.`,
		RunE: runServe,
	}

	cmd.AddCommand(newServeCommand())

	return cmd
}

func newServeCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the web UI server",
		RunE:  runServe,
	}

	cmd.Flags().StringVarP(&addr, "addr", "a", ":8080", "address to listen on")

	return cmd
}

func runServe(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	addr, _ := cmd.Flags().GetString("addr")
	if addr == "" {
		addr = ":8080"
	}

	dataPath := os.Getenv("TALKTOTHEM_DATA_PATH")
	if dataPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home directory: %w", err)
		}
		dataPath = filepath.Join(home, ".config", "talktothem")
	}

	if err := db.Init(dataPath); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}

	cfg := db.GetConfig()

	var llmClient *llm.Client
	if cfg.APIKey != "" {
		model := cfg.Model
		if model == "" {
			model = "gpt-4o"
		}
		llmClient = llm.NewClient(llm.Config{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   model,
		})
	}

	contacts, err := contact.NewManager(dataPath)
	if err != nil {
		return fmt.Errorf("create contact manager: %w", err)
	}

	var msgr messenger.Messenger
	signalAPIURL := os.Getenv("SIGNAL_API_URL")
	if signalAPIURL == "" {
		signalAPIURL = "http://localhost:8080"
	}

	// Check if Signal device is already linked and sync to DB
	signalClient := signalcli.NewWithoutNumber(signalAPIURL)
	linked, linkedNumber, err := signalClient.IsLinked(cmd.Context())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to check Signal link status: %v\n", err)
	}
	if linked && linkedNumber != "" {
		// Ensure DB is in sync with linked device
		signalCfg := db.GetMessengerConfig("signal")
		if signalCfg == nil || signalCfg.Phone != linkedNumber {
			fmt.Printf("Syncing Signal configuration for %s...\n", linkedNumber)
			signalCfg = &db.MessengerConfig{
				Type:    "signal",
				Phone:   linkedNumber,
				Enabled: true,
			}
			if err := db.SaveMessengerConfig(signalCfg); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to save Signal config: %v\n", err)
			}
		}
	}

	signalCfg := db.GetMessengerConfig("signal")
	if signalCfg != nil && signalCfg.Enabled && signalCfg.Phone != "" {
		msgr = signalcli.New(signalCfg.Phone, signalAPIURL)
		fmt.Println("Connecting to messenger...")
		if err := msgr.Connect(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to connect to the messenger: %v\n", err)
			fmt.Println("Continuing without a messenger connection...")
			msgr = nil
		} else {
			defer func() {
				fmt.Println("\nShutting down...")
				_ = msgr.Disconnect()
			}()
		}
	}

	ag := agent.New(llmClient, contacts, dataPath, agent.WithVision(llmClient))

	inbox := make(chan messenger.Message, 100)
	if llmClient != nil {
		go ag.Run(ctx, inbox)
	}

	server := api.NewServer(addr, ag, contacts, msgr, cfg, nil, signalAPIURL)

	// This needs to be after server is created so we can broadcast
	if msgr != nil {
		msgr.OnMessage(func(msg messenger.Message) {
			fmt.Printf("Received message from %s: %s\n", msg.ContactID, msg.Content)
			server.BroadcastMessage(msg)
			if err := ag.RecordMessage(context.Background(), msg); err != nil {
				fmt.Fprintf(os.Stderr, "Error recording message: %v\n", err)
			}
			if llmClient != nil {
				select {
				case inbox <- msg:
				default:
					fmt.Println("Inbox full, dropping message for agent")
				}
			}
		})
		msgr.StartReceiving(ctx)
	}

	fmt.Printf("Starting server on %s\n", addr)
	fmt.Println("Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	sig.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Run()
	}()

	select {
	case <-sigChan:
		fmt.Println("\nShutting down...")
		return server.Shutdown(ctx)
	case err := <-errChan:
		return err
	}
}
