package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/conversation"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

type Describer interface {
	Describe(ctx context.Context, imageData []byte) (string, error)
}

type Agent struct {
	llm             Generator
	describer       Describer
	contacts        *contact.Manager
	histories       map[string]*conversation.History
	historyMu       sync.RWMutex
	dataPath        string
	messageHandler  func(messenger.Message)
	reactionHandler func(messenger.Message)
}

func New(llm Generator, describer Describer, contacts *contact.Manager, dataPath string) *Agent {
	return &Agent{
		llm:       llm,
		describer: describer,
		contacts:  contacts,
		histories: make(map[string]*conversation.History),
		dataPath:  dataPath,
	}
}

func (a *Agent) OnMessage(handler func(messenger.Message)) {
	a.messageHandler = handler
}

func (a *Agent) OnReaction(handler func(messenger.Message)) {
	a.reactionHandler = handler
}

func (a *Agent) getHistory(contactID string) (*conversation.History, error) {
	a.historyMu.RLock()
	if h, ok := a.histories[contactID]; ok {
		a.historyMu.RUnlock()
		return h, nil
	}
	a.historyMu.RUnlock()

	a.historyMu.Lock()
	defer a.historyMu.Unlock()

	h, err := conversation.NewHistory(a.dataPath, contactID)
	if err != nil {
		return nil, err
	}
	a.histories[contactID] = h
	return h, nil
}

func (a *Agent) SyncHistory(ctx context.Context, m messenger.Messenger, contactID string) error {
	h, err := a.getHistory(contactID)
	if err != nil {
		return err
	}
	return h.Sync(ctx, m, contactID)
}

func (a *Agent) ProcessMessage(ctx context.Context, msg messenger.Message) (string, error) {
	c, ok := a.contacts.Get(msg.ContactID)
	if !ok || !c.Enabled {
		return "", nil
	}

	h, err := a.getHistory(msg.ContactID)
	if err != nil {
		return "", fmt.Errorf("failed to get history: %w", err)
	}

	if err := h.Add(msg); err != nil {
		return "", fmt.Errorf("failed to add message to history: %w", err)
	}

	return a.generateResponse(ctx, c, h, msg)
}

func (a *Agent) generateResponse(ctx context.Context, c contact.Contact, h *conversation.History, msg messenger.Message) (string, error) {
	recent := h.GetRecent(20)

	var promptBuilder strings.Builder
	promptBuilder.WriteString(buildSystemPrompt(c))
	promptBuilder.WriteString("\n\nConversation history:\n")

	for _, m := range recent {
		if m.IsFromMe {
			fmt.Fprintf(&promptBuilder, "You: %s\n", m.Content)
		} else {
			fmt.Fprintf(&promptBuilder, "%s: %s\n", c.Name, m.Content)
		}
	}

	if msg.Type == messenger.TypeImage && a.describer != nil {
		description, err := a.describer.Describe(ctx, []byte(msg.MediaURL))
		if err != nil {
			description = "[Unable to describe image]"
		}
		fmt.Fprintf(&promptBuilder, "\n%s sent an image: %s\n", c.Name, description)
	}

	fmt.Fprintf(&promptBuilder, "\n%s: %s\n", c.Name, msg.Content)
	promptBuilder.WriteString("\nRespond naturally as if you were the user:")

	return a.llm.Generate(ctx, promptBuilder.String())
}

func (a *Agent) GenerateReaction(ctx context.Context, msg messenger.Message) (string, error) {
	c, ok := a.contacts.Get(msg.ContactID)
	if !ok || !c.Enabled {
		return "", nil
	}

	prompt := fmt.Sprintf(`You are %s. Your contact %s sent: "%s"

What emoji reaction would you naturally give? Reply with ONLY a single emoji.`, "the user", c.Name, msg.Content)

	return a.llm.Generate(ctx, prompt)
}

func (a *Agent) LearnStyle(ctx context.Context, contactID string) (string, error) {
	h, err := a.getHistory(contactID)
	if err != nil {
		return "", err
	}

	messages := h.GetSince(time.Now().AddDate(0, -1, 0))
	if len(messages) == 0 {
		messages = h.GetRecent(100)
	}
	if len(messages) == 0 {
		return "No messages to learn from", nil
	}

	var myMessages []string
	for _, m := range messages {
		if m.IsFromMe {
			myMessages = append(myMessages, m.Content)
		}
	}

	if len(myMessages) == 0 {
		return "No user messages to learn from", nil
	}

	prompt := fmt.Sprintf(`Analyze these messages written by a user and describe their communication style:
%s

Describe the style in 2-3 sentences focusing on: tone, formality, emoji usage, message length, and any unique patterns.`, strings.Join(myMessages, "\n"))

	return a.llm.Generate(ctx, prompt)
}

func (a *Agent) ShouldRespond(contactID string, within time.Duration) (needsResponse bool, lastMessage messenger.Message, err error) {
	c, ok := a.contacts.Get(contactID)
	if !ok || !c.Enabled {
		return false, messenger.Message{}, nil
	}

	h, err := a.getHistory(contactID)
	if err != nil {
		return false, messenger.Message{}, fmt.Errorf("failed to get history: %w", err)
	}

	messages := h.GetRecent(1)
	if len(messages) == 0 {
		return false, messenger.Message{}, nil
	}

	lastMessage = messages[0]
	if lastMessage.IsFromMe {
		return false, lastMessage, nil
	}

	if time.Since(lastMessage.Timestamp) > within {
		return false, lastMessage, nil
	}

	return true, lastMessage, nil
}

func (a *Agent) InitiateConversation(ctx context.Context, contactID string) (string, error) {
	c, ok := a.contacts.Get(contactID)
	if !ok {
		return "", fmt.Errorf("contact not found: %s", contactID)
	}

	h, err := a.getHistory(contactID)
	if err != nil {
		return "", fmt.Errorf("failed to get history: %w", err)
	}

	recent := h.GetRecent(10)

	var promptBuilder strings.Builder
	promptBuilder.WriteString(buildSystemPrompt(c))

	if c.Style != "" {
		fmt.Fprintf(&promptBuilder, "\nYour communication style: %s\n", c.Style)
	}

	if len(recent) > 0 {
		promptBuilder.WriteString("\nRecent conversation history:\n")
		for _, m := range recent {
			if m.IsFromMe {
				fmt.Fprintf(&promptBuilder, "You: %s\n", m.Content)
			} else {
				fmt.Fprintf(&promptBuilder, "%s: %s\n", c.Name, m.Content)
			}
		}
	}

	promptBuilder.WriteString("\nInitiate a natural, casual message to start or continue this conversation. Keep it brief and appropriate. Reply with only the message:")

	return a.llm.Generate(ctx, promptBuilder.String())
}

func buildSystemPrompt(c contact.Contact) string {
	var prompt strings.Builder

	prompt.WriteString("You are roleplaying as the user. ")
	fmt.Fprintf(&prompt, "You are texting with %s. ", c.Name)

	if c.Description != "" {
		fmt.Fprintf(&prompt, "Context: %s. ", c.Description)
	}

	prompt.WriteString("Respond naturally and briefly as if you were the user. ")
	prompt.WriteString("Match the tone and style of previous messages. ")
	prompt.WriteString("Keep responses conversational and appropriate for a messaging app.")

	return prompt.String()
}

func (a *Agent) Start(ctx context.Context, m messenger.Messenger) error {
	m.OnMessage(func(msg messenger.Message) {
		if msg.IsFromMe {
			h, err := a.getHistory(msg.ContactID)
			if err == nil {
				_ = h.Add(msg)
			}
			return
		}

		response, err := a.ProcessMessage(ctx, msg)
		if err != nil {
			return
		}

		if response != "" && a.messageHandler != nil {
			msg.Content = response
			msg.Timestamp = time.Now()
			a.messageHandler(msg)
		}
	})

	m.OnReaction(func(msg messenger.Message) {
		if a.reactionHandler != nil {
			a.reactionHandler(msg)
		}
	})

	return nil
}
