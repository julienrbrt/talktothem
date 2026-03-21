package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	sig "os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/julienrbrt/talktothem/internal/agent"
	"github.com/julienrbrt/talktothem/internal/api"
	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/llm"
	"github.com/julienrbrt/talktothem/internal/messenger"
	signalcli "github.com/julienrbrt/talktothem/internal/messenger/signal"
	whatsappcli "github.com/julienrbrt/talktothem/internal/messenger/whatsapp"
	"github.com/lmittmann/tint"
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
	slog.SetDefault(slog.New(
		tint.NewHandler(os.Stderr, &tint.Options{
			Level:      slog.LevelDebug,
			TimeFormat: time.Kitchen,
		}),
	))

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

	signalAPIURL := os.Getenv("SIGNAL_API_URL")
	if signalAPIURL == "" {
		signalAPIURL = "http://localhost:8081"
	}

	msgrs := make(map[string]messenger.Messenger)

	// Add Signal
	signalMsgr := signalcli.NewWithoutNumber(signalAPIURL)
	msgrs[signalMsgr.Name()] = signalMsgr

	// Add WhatsApp (uses go-whatsapp-web-multidevice API)
	whatsappAPIURL := os.Getenv("WHATSAPP_API_URL")
	if whatsappAPIURL == "" {
		whatsappAPIURL = "http://localhost:3000"
	}
	whatsappMsgr, err := whatsappcli.New(dataPath, whatsappAPIURL)
	if err != nil {
		slog.Warn("Failed to initialize WhatsApp messenger", "error", err)
	} else {
		msgrs[whatsappMsgr.Name()] = whatsappMsgr
	}

	for name, m := range msgrs {
		// Check if messenger device is already linked and sync to DB
		linked, linkedNumber, err := m.IsLinked(cmd.Context())
		if err != nil {
			slog.Warn("failed to check messenger link status", "messenger", name, "error", err)
		}
		if linked && linkedNumber != "" {
			// Ensure DB is in sync with linked device
			cfg := db.GetMessengerConfig(name)
			if cfg == nil {
				slog.Info("Syncing messenger configuration", "messenger", name)
				cfg = &db.MessengerConfig{
					Type:    name,
					Enabled: true,
				}
			} else if !cfg.Enabled {
				slog.Info("Enabling linked messenger", "messenger", name)
				cfg.Enabled = true
			}

			if err := db.SaveMessengerConfig(cfg); err != nil {
				slog.Warn("failed to save messenger config", "messenger", name, "error", err)
			}
		}

		cfg := db.GetMessengerConfig(name)
		if cfg != nil && cfg.Enabled {
			slog.Info("Connecting to messenger...", "messenger", name)
			if err := m.Connect(ctx); err != nil {
				slog.Warn("failed to connect to the messenger", "messenger", name, "error", err)
				slog.Info("Continuing without a messenger connection...", "messenger", name)
			} else {
				// Capture the variable for defer
				mToClose := m
				nameToClose := name
				defer func() {
					slog.Info("Shutting down messenger...", "messenger", nameToClose)
					_ = mToClose.Disconnect()
				}()

				go importContactsOnStart(ctx, m, name, contacts)
				go prefillProfileOnStart(ctx, m, name)
			}
		}
	}

	ag := agent.New(llmClient, contacts, msgrs, dataPath, agent.WithVision(llmClient))

	inbox := make(chan messenger.Message, 100)
	if llmClient != nil {
		go ag.Run(ctx, inbox)
	}

	server := api.NewServer(ctx, addr, ag, contacts, msgrs, cfg, nil)

	// This needs to be after server is created so we can broadcast
	for _, m := range msgrs {
		m.OnMessage(func(msg messenger.Message) {
			slog.Info("Received message", "contactID", msg.ContactID, "content", msg.Content, "isGroup", msg.IsGroup)
			server.BroadcastMessage(msg)
			if err := ag.RecordMessage(context.Background(), msg); err != nil {
				slog.Error("Error recording message", "error", err)
			}
			if llmClient != nil {
				select {
				case inbox <- msg:
				default:
					slog.Warn("Inbox full, dropping message for agent")
				}
			}
		})
		m.OnReaction(func(msg messenger.Message) {
			slog.Info("Received reaction", "contactID", msg.ContactID, "reaction", msg.Reaction)
			server.BroadcastMessage(msg)
			if err := ag.RecordMessage(context.Background(), msg); err != nil {
				slog.Error("Error recording reaction", "error", err)
			}
		})
		m.StartReceiving(ctx)
	}

	slog.Info("Starting server", "addr", addr)
	slog.Info("Press Ctrl+C to stop.")

	sigChan := make(chan os.Signal, 1)
	sig.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Run()
	}()

	select {
	case <-sigChan:
		slog.Info("Shutting down...")
		return server.Shutdown(ctx)
	case err := <-errChan:
		return err
	}
}

func importContactsOnStart(ctx context.Context, msgr messenger.Messenger, name string, contacts *contact.Manager) {
	messengerContacts, err := msgr.GetContacts(ctx)
	if err != nil {
		slog.Warn("Failed to get contacts on start", "messenger", name, "error", err)
		return
	}

	var imported int
	for _, mc := range messengerContacts {
		if mc.Phone == "" {
			continue
		}

		existing, _ := contacts.Get(mc.Phone)
		if existing.ID != "" {
			continue
		}

		c := contact.Contact{
			ID:        mc.Phone,
			Name:      mc.Name,
			Phone:     mc.Phone,
			Messenger: name,
			Enabled:   false,
		}

		if err := contacts.Add(c); err != nil {
			continue
		}

		imported++
	}

	if imported > 0 {
		slog.Info("Imported contacts on start", "messenger", name, "count", imported)
	}
}

func prefillProfileOnStart(ctx context.Context, msgr messenger.Messenger, name string) {
	profile, err := msgr.GetOwnProfile(ctx)
	if err != nil {
		slog.Warn("Failed to get own profile on start", "messenger", name, "error", err)
		return
	}

	if profile.Name == "" && profile.About == "" {
		return
	}

	existing := db.GetUserProfile()
	updated := false

	if existing.Name == "" && profile.Name != "" {
		existing.Name = profile.Name
		updated = true
	}
	if existing.About == "" && profile.About != "" {
		existing.About = profile.About
		updated = true
	}

	if updated {
		if err := db.UpdateUserProfile(existing); err != nil {
			slog.Warn("Failed to pre-fill user profile on start", "messenger", name, "error", err)
			return
		}
		slog.Info("Pre-filled user profile on start", "messenger", name, "name", profile.Name, "about", profile.About)
	}
}
