package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/conversation"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

var (
	ErrContactNotFound   = errors.New("contact not found")
	ErrContactDisabled   = errors.New("contact disabled")
	ErrNoMessages        = errors.New("no messages to learn from")
	ErrNoUserMessages    = errors.New("no user messages to learn from")
	ErrNoResponseNeeded  = errors.New("no response needed")
)

type LLM interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

type Vision interface {
	Describe(ctx context.Context, imageData []byte) (string, error)
}

type Response struct {
	Content   string
	ContactID string
}

type ResponseCheck struct {
	Needed     bool
	LastSender string
	LastAt     time.Time
}

type Agent struct {
	llm       LLM
	vision    Vision
	contacts  *contact.Manager
	histories map[string]*conversation.History
	historyMu sync.RWMutex
	dataPath  string

	outbox chan Response
}

type Option func(*Agent)

func WithVision(v Vision) Option {
	return func(a *Agent) { a.vision = v }
}

func New(llm LLM, contacts *contact.Manager, dataPath string, opts ...Option) *Agent {
	a := &Agent{
		llm:       llm,
		contacts:  contacts,
		dataPath:  dataPath,
		histories: make(map[string]*conversation.History),
		outbox:    make(chan Response, 100),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *Agent) Outbox() <-chan Response { return a.outbox }

func (a *Agent) history(contactID string) (*conversation.History, error) {
	a.historyMu.RLock()
	if h, ok := a.histories[contactID]; ok {
		a.historyMu.RUnlock()
		return h, nil
	}
	a.historyMu.RUnlock()

	a.historyMu.Lock()
	defer a.historyMu.Unlock()

	if h, ok := a.histories[contactID]; ok {
		return h, nil
	}

	h, err := conversation.NewHistory(a.dataPath, contactID)
	if err != nil {
		return nil, err
	}
	a.histories[contactID] = h
	return h, nil
}

func (a *Agent) SyncHistory(ctx context.Context, m messenger.Messenger, contactID string) error {
	h, err := a.history(contactID)
	if err != nil {
		return err
	}
	return h.Sync(ctx, m, contactID)
}

func (a *Agent) Respond(ctx context.Context, msg messenger.Message) (string, error) {
	c, ok := a.contacts.Get(msg.ContactID)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrContactNotFound, msg.ContactID)
	}
	if !c.Enabled {
		return "", ErrContactDisabled
	}

	h, err := a.history(msg.ContactID)
	if err != nil {
		return "", fmt.Errorf("get history: %w", err)
	}

	if err := h.Add(msg); err != nil {
		return "", fmt.Errorf("add to history: %w", err)
	}

	return a.generateResponse(ctx, c, h, msg)
}

func (a *Agent) generateResponse(ctx context.Context, c contact.Contact, h *conversation.History, msg messenger.Message) (string, error) {
	recent := h.GetRecent(20)

	var b strings.Builder
	b.WriteString(systemPrompt(c))
	b.WriteString("\n\nConversation history:\n")

	for _, m := range recent {
		if m.IsFromMe {
			fmt.Fprintf(&b, "You: %s\n", m.Content)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", c.Name, m.Content)
		}
	}

	if msg.Type == messenger.TypeImage && a.vision != nil {
		desc, err := a.vision.Describe(ctx, []byte(msg.MediaURL))
		if err != nil {
			desc = "[Unable to describe image]"
		}
		fmt.Fprintf(&b, "\n%s sent an image: %s\n", c.Name, desc)
	}

	fmt.Fprintf(&b, "\n%s: %s\n", c.Name, msg.Content)
	b.WriteString("\nRespond naturally as if you were the user:")

	return a.llm.Generate(ctx, b.String())
}

func (a *Agent) LearnStyle(ctx context.Context, contactID string) (string, error) {
	h, err := a.history(contactID)
	if err != nil {
		return "", err
	}

	messages := h.GetSince(time.Now().AddDate(0, -1, 0))
	if len(messages) == 0 {
		messages = h.GetRecent(100)
	}
	if len(messages) == 0 {
		return "", ErrNoMessages
	}

	var mine []string
	for _, m := range messages {
		if m.IsFromMe {
			mine = append(mine, m.Content)
		}
	}

	if len(mine) == 0 {
		return "", ErrNoUserMessages
	}

	prompt := fmt.Sprintf(`Analyze these messages written by a user and describe their communication style:
%s

Describe the style in 2-3 sentences focusing on: tone, formality, emoji usage, message length, and any unique patterns.`, strings.Join(mine, "\n"))

	return a.llm.Generate(ctx, prompt)
}

func (a *Agent) CheckResponse(contactID string, within time.Duration) (ResponseCheck, error) {
	c, ok := a.contacts.Get(contactID)
	if !ok || !c.Enabled {
		return ResponseCheck{}, nil
	}

	h, err := a.history(contactID)
	if err != nil {
		return ResponseCheck{}, fmt.Errorf("get history: %w", err)
	}

	recent := h.GetRecent(1)
	if len(recent) == 0 {
		return ResponseCheck{}, nil
	}

	last := recent[0]
	check := ResponseCheck{
		LastAt:     last.Timestamp,
		LastSender: "them",
	}
	if last.IsFromMe {
		check.LastSender = "you"
		return check, nil
	}

	if time.Since(last.Timestamp) > within {
		return check, nil
	}

	check.Needed = true
	return check, nil
}

func (a *Agent) Initiate(ctx context.Context, contactID string) (string, error) {
	c, ok := a.contacts.Get(contactID)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrContactNotFound, contactID)
	}

	h, err := a.history(contactID)
	if err != nil {
		return "", fmt.Errorf("get history: %w", err)
	}

	recent := h.GetRecent(10)

	var b strings.Builder
	b.WriteString(systemPrompt(c))

	if c.Style != "" {
		fmt.Fprintf(&b, "\nYour communication style: %s\n", c.Style)
	}

	if len(recent) > 0 {
		b.WriteString("\nRecent conversation history:\n")
		for _, m := range recent {
			if m.IsFromMe {
				fmt.Fprintf(&b, "You: %s\n", m.Content)
			} else {
				fmt.Fprintf(&b, "%s: %s\n", c.Name, m.Content)
			}
		}
	}

	b.WriteString("\nInitiate a natural, casual message to start or continue this conversation. Keep it brief and appropriate. Reply with only the message:")

	return a.llm.Generate(ctx, b.String())
}

func (a *Agent) RecordMessage(ctx context.Context, msg messenger.Message) error {
	if !msg.IsFromMe {
		return nil
	}
	h, err := a.history(msg.ContactID)
	if err != nil {
		return err
	}
	return h.Add(msg)
}

func (a *Agent) Run(ctx context.Context, in <-chan messenger.Message) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-in:
			if !ok {
				return
			}
			if msg.IsFromMe {
				_ = a.RecordMessage(ctx, msg)
				continue
			}

			resp, err := a.Respond(ctx, msg)
			if err != nil {
				if errors.Is(err, ErrContactDisabled) || errors.Is(err, ErrContactNotFound) {
					continue
				}
				continue
			}

			if resp != "" {
				select {
				case a.outbox <- Response{Content: resp, ContactID: msg.ContactID}:
				default:
				}
			}
		}
	}
}

func systemPrompt(c contact.Contact) string {
	var b strings.Builder
	b.WriteString("You are roleplaying as the user. ")
	fmt.Fprintf(&b, "You are texting with %s. ", c.Name)
	if c.Description != "" {
		fmt.Fprintf(&b, "Context: %s. ", c.Description)
	}
	b.WriteString("Respond naturally and briefly as if you were the user. ")
	b.WriteString("Match the tone and style of previous messages. ")
	b.WriteString("Keep responses conversational and appropriate for a messaging app.")
	return b.String()
}
