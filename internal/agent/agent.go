package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/conversation"
	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

var (
	ErrContactNotFound  = errors.New("contact not found")
	ErrContactDisabled  = errors.New("contact disabled")
	ErrNoMessages       = errors.New("no messages to learn from")
	ErrNoUserMessages   = errors.New("no user messages to learn from")
	ErrNoResponseNeeded = errors.New("no response needed")
)

const (
	maxUserStyleSnapshot = 100
	summaryFallbackCount = 50
)

type LLM interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

type Vision interface {
	Describe(ctx context.Context, imageData []byte) (string, error)
}

type Response struct {
	Content          string
	ContactID        string
	Delay            time.Duration
	TypingDelay      time.Duration
	TriggerMessageID string
}

type QueuedResponse struct {
	Content     string
	ContactID   string
	SendAt      time.Time
	TypingDelay time.Duration
}

type ResponseCheck struct {
	Needed     bool
	LastSender string
	LastAt     time.Time
}

type Agent struct {
	llm        LLM
	vision     Vision
	contacts   *contact.Manager
	histories  map[string]*conversation.History
	historyMu  sync.RWMutex
	dataPath   string
	cancels    sync.Map // contactID -> context.CancelFunc
	messengers map[string]messenger.Messenger

	outbox chan Response
	queued chan QueuedResponse
}

type Option func(*Agent)

func WithVision(v Vision) Option {
	return func(a *Agent) { a.vision = v }
}

func New(llm LLM, contacts *contact.Manager, messengers map[string]messenger.Messenger, dataPath string, opts ...Option) *Agent {
	a := &Agent{
		llm:        llm,
		contacts:   contacts,
		messengers: messengers,
		dataPath:   dataPath,
		histories:  make(map[string]*conversation.History),
		outbox:     make(chan Response, 100),
		queued:     make(chan QueuedResponse, 100),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func (a *Agent) Outbox() <-chan Response       { return a.outbox }
func (a *Agent) Queued() <-chan QueuedResponse { return a.queued }

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

func (a *Agent) SyncAllHistory(ctx context.Context, messengerName string) (int, error) {
	msgr, ok := a.messengers[messengerName]
	if !ok || msgr == nil || !msgr.IsConnected() {
		return 0, nil
	}

	contacts := a.contacts.List()
	synced := 0

	for _, c := range contacts {
		if c.Messenger != messengerName {
			continue
		}
		if err := a.SyncHistory(ctx, msgr, c.ID); err != nil {
			slog.Warn("Failed to sync history for contact", "contact", c.Name, "error", err)
			continue
		}
		synced++
		slog.Info("Synced history for contact", "contact", c.Name, "messenger", messengerName)
	}

	return synced, nil
}

func (a *Agent) Respond(ctx context.Context, msg messenger.Message) (string, error) {
	c, ok := a.contacts.Get(msg.ContactID)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrContactNotFound, msg.ContactID)
	}
	if !c.Enabled {
		return "", ErrContactDisabled
	}

	if err := a.markRead(ctx, c, msg); err != nil {
		slog.Error("Agent Failed to mark message as read", "error", err)
	}

	h, err := a.history(msg.ContactID)
	if err != nil {
		return "", fmt.Errorf("get history: %w", err)
	}

	resp, err := a.generateResponse(ctx, c, h, msg)
	if err != nil {
		return "", err
	}

	if emoji, found := strings.CutPrefix(resp, "REACTION: "); found {
		emoji = strings.TrimSpace(emoji)
		if err := a.sendReaction(ctx, c, msg, emoji); err != nil {
			slog.Error("Agent Failed to send reaction", "error", err)
		}
		return "", nil
	}

	return resp, nil
}

func (a *Agent) markRead(ctx context.Context, c contact.Contact, msg messenger.Message) error {
	if msg.ID == "" {
		return nil
	}
	msgr, ok := a.messengers[c.Messenger]
	if !ok || msgr == nil {
		return nil
	}
	return msgr.MarkRead(ctx, msg.ContactID, []string{msg.ID})
}

func (a *Agent) sendReaction(ctx context.Context, c contact.Contact, msg messenger.Message, emoji string) error {
	msgr, ok := a.messengers[c.Messenger]
	if !ok || msgr == nil {
		return nil
	}

	if err := msgr.SendReaction(ctx, msg.ContactID, msg.ID, emoji); err != nil {
		return err
	}

	return a.RecordMessage(ctx, messenger.Message{
		ContactID: msg.ContactID,
		Type:      messenger.TypeReaction,
		Reaction:  emoji,
		Timestamp: time.Now(),
		IsFromMe:  true,
	})
}

func (a *Agent) generateResponse(ctx context.Context, c contact.Contact, h *conversation.History, msg messenger.Message) (string, error) {
	recent := h.GetRecent(20)
	profile := db.GetUserProfile()

	var b strings.Builder
	b.WriteString(systemPrompt(c, profile))

	appendStyleContext(&b, c, profile, recent)

	b.WriteString("\nConversation history:\n")
	for _, m := range recent {
		prefix := fmt.Sprintf("%s: ", c.Name)
		if m.IsFromMe {
			prefix = "You: "
		}

		switch m.Type {
		case messenger.TypeImage:
			fmt.Fprintf(&b, "%s[Sent an image]\n", prefix)
		case messenger.TypeSticker:
			fmt.Fprintf(&b, "%s[Sent a sticker]\n", prefix)
		case messenger.TypeReaction:
			fmt.Fprintf(&b, "%s[Reacted with %s]\n", prefix, m.Reaction)
		default:
			fmt.Fprintf(&b, "%s%s\n", prefix, m.Content)
		}
	}

	if a.vision != nil {
		for i, path := range msg.MediaURLs {
			data, err := os.ReadFile(path)
			if err != nil {
				slog.Error("Agent Failed to read media file", "path", path, "error", err)
				continue
			}

			desc, err := a.vision.Describe(ctx, data)
			if err != nil {
				desc = "[Unable to describe]"
			}
			msgType := "image"
			if msg.Type == messenger.TypeSticker {
				msgType = "sticker"
			}
			fmt.Fprintf(&b, "\n%s sent %s %d: %s\n", c.Name, msgType, i+1, desc)
		}
	}

	fmt.Fprintf(&b, "\n%s: %s\n", c.Name, msg.Content)
	b.WriteString("\nReply now as you would naturally text them. Look at the conversation history and your communication profile above. ")
	if profile.Language != "" {
		fmt.Fprintf(&b, "Write in the same language %s is using. If unclear, write in %s. ", c.Name, profile.Language)
	} else {
		fmt.Fprintf(&b, "Write in the same language %s is using. ", c.Name)
	}
	b.WriteString("Match the other person's energy and tone. Match your own typical message length and punctuation habits. ")
	b.WriteString("If a reaction emoji is more natural than text (e.g. they shared something and you'd just react), reply with 'REACTION: ' followed by the emoji. ")
	b.WriteString("One message only. No preamble. No explanation. Just your reply:")

	return a.llm.Generate(ctx, b.String())
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
	profile := db.GetUserProfile()

	var b strings.Builder
	b.WriteString(systemPrompt(c, profile))

	appendStyleContext(&b, c, profile, recent)

	if len(recent) > 0 {
		b.WriteString("\nRecent conversation:\n")
		for _, m := range recent {
			if m.IsFromMe {
				fmt.Fprintf(&b, "You: %s\n", m.Content)
			} else {
				fmt.Fprintf(&b, "%s: %s\n", c.Name, m.Content)
			}
		}
	}

	b.WriteString("\nStart or continue this conversation naturally. Look at your communication profile above. ")
	if profile.Language != "" {
		fmt.Fprintf(&b, "Write in the same language %s uses. If no history exists, write in %s. ", c.Name, profile.Language)
	} else {
		fmt.Fprintf(&b, "Write in the same language %s uses. ", c.Name)
	}
	b.WriteString("Text them the way you normally would — your usual greeting style, punctuation, emoji habits. ")
	b.WriteString("One message only. No preamble. No explanation. Just your message:")

	return a.llm.Generate(ctx, b.String())
}

func (a *Agent) RecordMessage(ctx context.Context, msg messenger.Message) error {
	h, err := a.history(msg.ContactID)
	if err != nil {
		return err
	}
	return h.Add(msg)
}

func (a *Agent) ClearHistory(contactID string) error {
	h, err := a.history(contactID)
	if err != nil {
		return err
	}
	return h.Clear()
}

func (a *Agent) SetLLM(llm LLM) {
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	a.llm = llm
}

func (a *Agent) SetVision(v Vision) {
	a.historyMu.Lock()
	defer a.historyMu.Unlock()
	a.vision = v
}

func (a *Agent) HasLLM() bool {
	a.historyMu.RLock()
	defer a.historyMu.RUnlock()
	return a.llm != nil
}

func (a *Agent) Summarize(ctx context.Context, contactID string, before time.Time) (string, error) {
	h, err := a.history(contactID)
	if err != nil {
		return "", err
	}

	startOfDay := time.Date(before.Year(), before.Month(), before.Day(), 0, 0, 0, 0, before.Location())
	messages := h.GetRange(0, startOfDay, before)
	if len(messages) == 0 {
		messages = h.GetBefore(summaryFallbackCount, before)
	}

	if len(messages) == 0 {
		return "No conversation history found.", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Summarize the conversation history as of %s:\n\n", before.Format("2006-01-02 15:04"))
	for _, m := range messages {
		if m.IsFromMe {
			fmt.Fprintf(&b, "You: %s\n", m.Content)
		} else {
			fmt.Fprintf(&b, "Contact: %s\n", m.Content)
		}
	}
	b.WriteString("\nProvide a concise 1-2 sentence summary of the conversation state at that time.")

	return a.llm.Generate(ctx, b.String())
}

func (a *Agent) Run(ctx context.Context, in <-chan messenger.Message) {
	slog.Info("Agent Run loop started")
	for {
		select {
		case <-ctx.Done():
			slog.Info("Agent Context done, stopping")
			return
		case msg, ok := <-in:
			if !ok {
				slog.Info("Agent Inbox closed, stopping")
				return
			}

			if !a.HasLLM() {
				slog.Info("Agent No LLM configured, skipping")
				continue
			}

			slog.Info("Agent Received message", "contactID", msg.ContactID, "content", msg.Content, "isGroup", msg.IsGroup)

			// Skip group messages to prevent mixing with normal conversations
			if msg.IsGroup {
				slog.Info("Agent Group message received, skipping", "contactID", msg.ContactID)
				continue
			}

			if msg.IsFromMe {
				slog.Info("Agent Message is from me, skipping generation")
				continue
			}

			if time.Since(msg.Timestamp) > 2*time.Minute {
				slog.Info("Agent Skipping stale message", "contactID", msg.ContactID, "age", time.Since(msg.Timestamp))
				continue
			}

			// Cancel any previous pending response for this contact
			a.Stop(msg.ContactID)

			// Start a new response goroutine
			msgCtx, cancel := context.WithCancel(ctx)
			a.cancels.Store(msg.ContactID, cancel)

			go func(cID string, m messenger.Message) {
				defer a.cancels.Delete(cID)
				defer cancel()

				resp, err := a.Respond(msgCtx, m)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						slog.Error("Agent Respond error", "error", err)
					}
					return
				}

				if resp != "" {
					slog.Info("Agent Generated response", "response", resp)

					delay := a.calculateDelay(m, resp)
					typingDelay := a.calculateTypingDelay(resp)
					sendAt := time.Now().Add(delay)

					select {
					case a.queued <- QueuedResponse{Content: resp, ContactID: cID, SendAt: sendAt, TypingDelay: typingDelay}:
					default:
					}

					// Wait for delay or cancellation
					timer := time.NewTimer(delay)
					defer timer.Stop()

					select {
					case <-msgCtx.Done():
						slog.Info("Agent Response canceled", "contactID", cID)
						return
					case <-timer.C:
						select {
						case a.outbox <- Response{Content: resp, ContactID: cID, TypingDelay: typingDelay, TriggerMessageID: m.ID}:
							slog.Info("Agent Response sent to outbox")
						case <-msgCtx.Done():
							return
						default:
							slog.Warn("Agent Outbox full, dropping response")
						}
					}
				}
			}(msg.ContactID, msg)
		}
	}
}

func (a *Agent) Stop(contactID string) {
	if cancel, ok := a.cancels.Load(contactID); ok {
		cancel.(context.CancelFunc)()
		a.cancels.Delete(contactID)
	}
}

func (a *Agent) calculateDelay(lastMsg messenger.Message, response string) time.Duration {
	cfg := db.GetConfig()
	if cfg != nil && cfg.DisableDelay {
		return 0
	}

	// Base delay: 2-5 seconds for "thinking" before typing starts
	delay := time.Duration(2+(time.Now().UnixNano()%3)) * time.Second

	// Add typing time: ~200 chars per minute = ~3 chars per second, max 30s
	typingTime := time.Duration(min(len(response)/3, 30)) * time.Second
	delay += typingTime

	// If the last message was very recent, add more delay to seem natural
	timeSinceLast := time.Since(lastMsg.Timestamp)
	if timeSinceLast < 10*time.Second {
		delay += 10 * time.Second
	}

	return delay
}

func (a *Agent) calculateTypingDelay(response string) time.Duration {
	cfg := db.GetConfig()
	if cfg != nil && cfg.DisableDelay {
		return 0
	}

	// ~300 chars per minute = ~5 chars per second, scaled by message length, max 20s
	typingTime := max(time.Duration(min(len(response)/5, 20))*time.Second, time.Second)
	return typingTime
}

func systemPrompt(c contact.Contact, profile *db.UserProfile) string {
	var b strings.Builder
	b.WriteString("You are ")
	if profile.Name != "" {
		b.WriteString(profile.Name)
	} else {
		b.WriteString("the user")
	}
	b.WriteString(". You are NOT an AI. You are a real person texting from your phone. This is not a roleplay — you ARE this person. ")

	fmt.Fprintf(&b, "You are texting with %s. ", c.Name)
	if c.Relation != "" {
		fmt.Fprintf(&b, "They are your %s. ", c.Relation)
	}
	if c.Description != "" {
		fmt.Fprintf(&b, "Context about them: %s. ", c.Description)
	}
	if c.BannedTopics != "" {
		fmt.Fprintf(&b, "NEVER mention or discuss these topics: %s. ", c.BannedTopics)
	}

	if profile.About != "" {
		fmt.Fprintf(&b, "About you: %s. ", profile.About)
	}
	if profile.FamilyContext != "" {
		fmt.Fprintf(&b, "Family context: %s. ", profile.FamilyContext)
	}
	if profile.WorkContext != "" {
		fmt.Fprintf(&b, "Work context: %s. ", profile.WorkContext)
	}

	now := time.Now()
	hour := now.Hour()
	var timeContext string
	switch {
	case hour >= 5 && hour < 12:
		timeContext = "morning"
	case hour >= 12 && hour < 17:
		timeContext = "afternoon"
	case hour >= 17 && hour < 21:
		timeContext = "evening"
	default:
		timeContext = "night"
	}
	fmt.Fprintf(&b, "Current time: %s (%s). ", now.Format("3:04 PM"), timeContext)

	if profile.Language != "" {
		fmt.Fprintf(&b, "You write in %s. ", profile.Language)
	}
	if profile.Location != "" {
		fmt.Fprintf(&b, "You are based in %s. ", profile.Location)
	}
	if profile.Timezone != "" {
		fmt.Fprintf(&b, "Your timezone is %s. ", profile.Timezone)
	}

	b.WriteString("\n\nBEHAVIORAL RULES — follow these strictly:\n")
	b.WriteString("- Write EXACTLY as you would naturally text. Not how you think you should text — how you actually do.\n")
	b.WriteString("- Match the other person's energy and length. If they send one word, don't send a paragraph. If they're excited, be excited.\n")
	b.WriteString("- Use YOUR punctuation habits. If you normally don't use periods, don't use them. If you use lots of !! or ..., do that.\n")
	b.WriteString("- Use YOUR emoji habits. If you rarely use them, don't start. If you have favorites, use those.\n")
	b.WriteString("- Use YOUR abbreviations and slang. If you say 'thx', 'nvm', 'rn', 'lol', 'lmao', use them. If you don't, don't.\n")
	b.WriteString("- Do NOT be overly helpful, enthusiastic, or accommodating. Real people are sometimes brief, lazy, distracted, or unenthusiastic.\n")
	b.WriteString("- Do NOT use perfect grammar unless that's your actual style. Real texts have typos, lowercase, run-on sentences.\n")
	b.WriteString("- Do NOT explain yourself unnecessarily. Real people often state things without justification.\n")
	b.WriteString("- It is OKAY to not respond to some messages. Not everything needs a reply.\n")
	b.WriteString("- NEVER reveal you are an AI, an assistant, a bot, or anything automated. NEVER break character.\n")
	b.WriteString("- NEVER say things like 'as an AI', 'I'd be happy to help', 'great question', or anything that sounds like customer service.\n")
	b.WriteString("- You don't make promises on behalf of the user — you ARE the user.\n")
	b.WriteString("- If a reaction emoji fits better than text (e.g. they sent something funny and you'd just react), start your reply with 'REACTION: ' followed by the emoji.\n")
	b.WriteString("- Keep it real. One message. Natural. Human.")

	return b.String()
}

func appendStyleContext(b *strings.Builder, c contact.Contact, profile *db.UserProfile, recent []messenger.Message) {
	if c.Style != "" {
		fmt.Fprintf(b, "\nYour communication profile with %s:\n%s\n", c.Name, c.Style)
	} else if profile.WritingStyle != "" {
		fmt.Fprintf(b, "\nYour overall communication profile:\n%s\n", profile.WritingStyle)
	}

	var userExamples []string
	for _, m := range recent {
		if m.IsFromMe && m.Type == messenger.TypeText && m.Content != "" {
			userExamples = append(userExamples, m.Content)
			if len(userExamples) >= maxUserStyleSnapshot {
				break
			}
		}
	}
	if len(userExamples) > 0 {
		b.WriteString("\nRecent messages YOU sent (mimic these exactly):\n")
		for _, ex := range userExamples {
			fmt.Fprintf(b, "  %s\n", ex)
		}
	}
}
