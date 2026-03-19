package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	sig "os/signal"
	"strings"
	"syscall"
	"time"

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

	cmd.AddCommand(newRunCommand())
	cmd.AddCommand(newConfigCommand())

	return cmd
}

type conversationInfo struct {
	contact      messenger.Contact
	lastMessage  messenger.Message
	messageCount int
}

func newRunCommand() *cobra.Command {
	var (
		dryRun         bool
		contactArg     string
		responseWindow time.Duration
		autoInitiate   bool
	)

	cmd := &cobra.Command{
		Use:   "run [contact-name]",
		Short: "Run the agent for a conversation",
		Long: `Run the agent for a single conversation.

If no contact is specified, lists available conversations and prompts for selection.
Automatically syncs history, learns style, and responds or initiates as needed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				contactArg = args[0]
			}

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

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			if dryRun {
				fmt.Println("Dry run mode - not connecting to Signal")
				return nil
			}

			fmt.Println("Connecting to Signal...")
			if err := signalClient.Connect(ctx); err != nil {
				return fmt.Errorf("failed to connect to Signal: %w", err)
			}

			conversations, err := listConversations(ctx, signalClient, contacts)
			if err != nil {
				signalClient.Disconnect()
				return fmt.Errorf("failed to list conversations: %w", err)
			}

			if len(conversations) == 0 {
				signalClient.Disconnect()
				return fmt.Errorf("no conversations found")
			}

			var selected *conversationInfo
			if contactArg != "" {
				for i, conv := range conversations {
					if conv.contact.Phone == contactArg ||
						strings.EqualFold(conv.contact.Name, contactArg) {
						selected = &conversations[i]
						break
					}
				}
				if selected == nil {
					signalClient.Disconnect()
					return fmt.Errorf("contact not found: %s", contactArg)
				}
			} else {
				selected, err = promptContactSelection(conversations)
				if err != nil {
					signalClient.Disconnect()
					return err
				}
			}

			fmt.Printf("\nSelected: %s (%s)\n", selected.contact.Name, selected.contact.Phone)

			if err := contacts.SetEnabled(selected.contact.Phone, true); err != nil {
				signalClient.Disconnect()
				return fmt.Errorf("failed to enable contact: %w", err)
			}

			fmt.Println("Syncing history...")
			if err := a.SyncHistory(ctx, signalClient, selected.contact.Phone); err != nil {
				signalClient.Disconnect()
				return fmt.Errorf("failed to sync history: %w", err)
			}

			fmt.Println("Learning your style...")
			style, err := a.LearnStyle(ctx, selected.contact.Phone)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to learn style: %v\n", err)
			} else {
				fmt.Printf("Learned style: %s\n", style)
				if err := contacts.SetStyle(selected.contact.Phone, style); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to save style: %v\n", err)
				}
			}

			needsResponse, lastMsg, err := a.ShouldRespond(selected.contact.Phone, responseWindow)
			if err != nil {
				signalClient.Disconnect()
				return fmt.Errorf("failed to check response need: %w", err)
			}

			if needsResponse {
				fmt.Printf("\nResponding to recent message from %s...\n", lastMsg.Timestamp.Format("2006-01-02 15:04"))
				response, err := a.ProcessMessage(ctx, lastMsg)
				if err != nil {
					signalClient.Disconnect()
					return fmt.Errorf("failed to generate response: %w", err)
				}
				if response != "" {
					fmt.Printf("Sending: %s\n", response)
					if err := signalClient.SendMessage(ctx, selected.contact.Phone, response); err != nil {
						signalClient.Disconnect()
						return fmt.Errorf("failed to send message: %w", err)
					}
				}
			} else if autoInitiate {
				fmt.Println("\nInitiating conversation...")
				message, err := a.InitiateConversation(ctx, selected.contact.Phone)
				if err != nil {
					signalClient.Disconnect()
					return fmt.Errorf("failed to initiate: %w", err)
				}
				if message != "" {
					fmt.Printf("Sending: %s\n", message)
					if err := signalClient.SendMessage(ctx, selected.contact.Phone, message); err != nil {
						signalClient.Disconnect()
						return fmt.Errorf("failed to send message: %w", err)
					}
				}
			} else {
				fmt.Println("\nNo recent message requiring response. Listening for new messages...")
			}

			a.OnMessage(func(msg messenger.Message) {
				fmt.Printf("[%s] Sending: %s\n", msg.ContactID, msg.Content)
				if err := signalClient.SendMessage(ctx, msg.ContactID, msg.Content); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to send message: %v\n", err)
				}
			})

			fmt.Println("\nAgent running. Press Ctrl+C to stop.")

			if err := a.Start(ctx, signalClient); err != nil {
				signalClient.Disconnect()
				return fmt.Errorf("failed to start agent: %w", err)
			}

			sigChan := make(chan os.Signal, 1)
			sig.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			<-sigChan

			fmt.Println("\nShutting down...")
			_ = contacts.SetEnabled(selected.contact.Phone, false)
			return signalClient.Disconnect()
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Run without sending messages")
	cmd.Flags().DurationVar(&responseWindow, "response-window", 24*time.Hour, "Time window to consider messages as needing response")
	cmd.Flags().BoolVar(&autoInitiate, "initiate", false, "Initiate conversation if no response needed")

	return cmd
}

func listConversations(ctx context.Context, m messenger.Messenger, contacts *contact.Manager) ([]conversationInfo, error) {
	signalContacts, err := m.GetContacts(ctx)
	if err != nil {
		return nil, err
	}

	var conversations []conversationInfo
	for _, sc := range signalContacts {
		msgs, err := m.GetConversation(ctx, sc.Phone, 1)
		if err != nil {
			continue
		}

		if len(msgs) == 0 {
			continue
		}

		allMsgs, err := m.GetConversation(ctx, sc.Phone, 0)
		if err != nil {
			continue
		}

		c, _ := contacts.Get(sc.Phone)
		if c.Phone == "" {
			c = contact.Contact{
				ID:      sc.Phone,
				Phone:   sc.Phone,
				Name:    sc.Name,
				Enabled: false,
			}
		}
		if c.Name == "" && sc.Name != "" {
			c.Name = sc.Name
		}

		conversations = append(conversations, conversationInfo{
			contact: messenger.Contact{
				ID:      c.Phone,
				Name:    c.Name,
				Phone:   c.Phone,
				Enabled: c.Enabled,
			},
			lastMessage:  msgs[0],
			messageCount: len(allMsgs),
		})
	}

	return conversations, nil
}

func promptContactSelection(conversations []conversationInfo) (*conversationInfo, error) {
	fmt.Println("\nAvailable conversations:")
	fmt.Println(strings.Repeat("-", 60))
	for i, conv := range conversations {
		status := ""
		if conv.contact.Enabled {
			status = " [enabled]"
		}
		lastMsgTime := conv.lastMessage.Timestamp.Format("2006-01-02 15:04")
		from := "them"
		if conv.lastMessage.IsFromMe {
			from = "you"
		}
		fmt.Printf("%2d. %s%s\n    Last: %s (%s) - %d messages\n",
			i+1, conv.contact.Name, status, lastMsgTime, from, conv.messageCount)
	}
	fmt.Println(strings.Repeat("-", 60))

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Select conversation (1-" + fmt.Sprint(len(conversations)) + "): ")
	input, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read input: %w", err)
	}

	input = strings.TrimSpace(input)
	var selection int
	if _, err := fmt.Sscanf(input, "%d", &selection); err != nil {
		return nil, fmt.Errorf("invalid selection: %s", input)
	}

	if selection < 1 || selection > len(conversations) {
		return nil, fmt.Errorf("selection out of range: %d", selection)
	}

	return &conversations[selection-1], nil
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
