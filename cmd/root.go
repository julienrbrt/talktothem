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
	signalcli "github.com/julienrbrt/talktothem/internal/messenger/signal"
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

func newRunCommand() *cobra.Command {
	var (
		dryRun         bool
		responseWindow time.Duration
		initiate       bool
	)

	cmd := &cobra.Command{
		Use:   "run [contact]",
		Short: "Run the agent for a conversation",
		Long: `Run the agent for a single conversation.

If no contact is specified, lists available conversations and prompts for selection.
Automatically syncs history, learns style, and responds or initiates as needed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			cfg, err := config.Load(cfgFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			llmClient, err := createLLMClient(cfg)
			if err != nil {
				return err
			}

			contacts, err := contact.NewManager(cfg.Contact.DataPath)
			if err != nil {
				return fmt.Errorf("create contact manager: %w", err)
			}

			msgr := signalcli.New(cfg.Signal.PhoneNumber, signalcli.WithDataPath(cfg.Signal.DataPath))

			if dryRun {
				return runDryRun()
			}

			fmt.Println("Connecting to Signal...")
			if err := msgr.Connect(ctx); err != nil {
				return fmt.Errorf("connect to Signal: %w", err)
			}
			defer func() {
				fmt.Println("\nShutting down...")
				_ = msgr.Disconnect()
			}()

			convs, err := listConversations(ctx, msgr, contacts)
			if err != nil {
				return fmt.Errorf("list conversations: %w", err)
			}
			if len(convs) == 0 {
				return fmt.Errorf("no conversations found")
			}

			var selected *conversation
			if len(args) > 0 {
				selected = findConversation(convs, args[0])
				if selected == nil {
					return fmt.Errorf("contact not found: %s", args[0])
				}
			} else {
				selected, err = promptSelection(convs)
				if err != nil {
					return err
				}
			}

			fmt.Printf("\nSelected: %s (%s)\n", selected.contact.Name, selected.contact.Phone)

			if err := contacts.SetEnabled(selected.contact.Phone, true); err != nil {
				return fmt.Errorf("enable contact: %w", err)
			}
			defer func() { _ = contacts.SetEnabled(selected.contact.Phone, false) }()

			ag := agent.New(llmClient, contacts, cfg.Contact.DataPath, agent.WithVision(llmClient))

			fmt.Println("Syncing history...")
			if err := ag.SyncHistory(ctx, msgr, selected.contact.Phone); err != nil {
				return fmt.Errorf("sync history: %w", err)
			}

			fmt.Println("Learning your style...")
			if style, err := ag.LearnStyle(ctx, selected.contact.Phone); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to learn style: %v\n", err)
			} else {
				fmt.Printf("Learned style: %s\n", style)
				if err := contacts.SetStyle(selected.contact.Phone, style); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to save style: %v\n", err)
				}
			}

			check, err := ag.CheckResponse(selected.contact.Phone, responseWindow)
			if err != nil {
				return fmt.Errorf("check response: %w", err)
			}

			switch {
			case check.Needed:
				fmt.Printf("\nResponding to message from %s...\n", check.LastAt.Format("2006-01-02 15:04"))
				lastMsg := selected.lastMessage
				resp, err := ag.Respond(ctx, lastMsg)
				if err != nil {
					return fmt.Errorf("generate response: %w", err)
				}
				if resp != "" {
					fmt.Printf("Sending: %s\n", resp)
					if err := msgr.SendMessage(ctx, selected.contact.Phone, resp); err != nil {
						return fmt.Errorf("send message: %w", err)
					}
				}

			case initiate:
				fmt.Println("\nInitiating conversation...")
				msg, err := ag.Initiate(ctx, selected.contact.Phone)
				if err != nil {
					return fmt.Errorf("initiate: %w", err)
				}
				if msg != "" {
					fmt.Printf("Sending: %s\n", msg)
					if err := msgr.SendMessage(ctx, selected.contact.Phone, msg); err != nil {
						return fmt.Errorf("send message: %w", err)
					}
				}

			default:
				fmt.Println("\nNo recent message requiring response. Listening for new messages...")
			}

			inbox := make(chan messenger.Message, 100)
			msgr.OnMessage(func(msg messenger.Message) {
				select {
				case inbox <- msg:
				default:
				}
			})

			go ag.Run(ctx, inbox)

			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case resp := <-ag.Outbox():
						fmt.Printf("[%s] Sending: %s\n", resp.ContactID, resp.Content)
						if err := msgr.SendMessage(ctx, resp.ContactID, resp.Content); err != nil {
							fmt.Fprintf(os.Stderr, "Failed to send: %v\n", err)
						}
					}
				}
			}()

			fmt.Println("\nAgent running. Press Ctrl+C to stop.")

			sigChan := make(chan os.Signal, 1)
			sig.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			<-sigChan

			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Run without sending messages")
	cmd.Flags().DurationVar(&responseWindow, "response-window", 24*time.Hour, "Time window to consider messages as needing response")
	cmd.Flags().BoolVar(&initiate, "initiate", false, "Initiate conversation if no response needed")

	return cmd
}

func runDryRun() error {
	fmt.Println("Dry run mode - not connecting to Signal")
	return nil
}

type conversation struct {
	contact      messenger.Contact
	lastMessage  messenger.Message
	messageCount int
}

func listConversations(ctx context.Context, m messenger.Messenger, contacts *contact.Manager) ([]conversation, error) {
	signalContacts, err := m.GetContacts(ctx)
	if err != nil {
		return nil, err
	}

	var convs []conversation
	for _, sc := range signalContacts {
		msgs, err := m.GetConversation(ctx, sc.Phone, 1)
		if err != nil || len(msgs) == 0 {
			continue
		}

		all, err := m.GetConversation(ctx, sc.Phone, 0)
		if err != nil {
			continue
		}

		c, _ := contacts.Get(sc.Phone)
		name := c.Name
		if name == "" {
			name = sc.Name
		}
		if name == "" {
			name = sc.Phone
		}

		convs = append(convs, conversation{
			contact: messenger.Contact{
				ID:      sc.Phone,
				Name:    name,
				Phone:   sc.Phone,
				Enabled: c.Enabled,
			},
			lastMessage:  msgs[0],
			messageCount: len(all),
		})
	}

	return convs, nil
}

func findConversation(convs []conversation, query string) *conversation {
	for i, c := range convs {
		if c.contact.Phone == query || strings.EqualFold(c.contact.Name, query) {
			return &convs[i]
		}
	}
	return nil
}

func promptSelection(convs []conversation) (*conversation, error) {
	fmt.Println("\nAvailable conversations:")
	fmt.Println(strings.Repeat("-", 60))
	for i, c := range convs {
		status := ""
		if c.contact.Enabled {
			status = " [enabled]"
		}
		sender := "them"
		if c.lastMessage.IsFromMe {
			sender = "you"
		}
		fmt.Printf("%2d. %s%s\n    Last: %s (%s) - %d messages\n",
			i+1, c.contact.Name, status,
			c.lastMessage.Timestamp.Format("2006-01-02 15:04"), sender, c.messageCount)
	}
	fmt.Println(strings.Repeat("-", 60))

	fmt.Printf("Select conversation (1-%d): ", len(convs))
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}

	var sel int
	if _, err := fmt.Sscanf(strings.TrimSpace(input), "%d", &sel); err != nil {
		return nil, fmt.Errorf("invalid selection: %s", strings.TrimSpace(input))
	}
	if sel < 1 || sel > len(convs) {
		return nil, fmt.Errorf("selection out of range: %d", sel)
	}

	return &convs[sel-1], nil
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
