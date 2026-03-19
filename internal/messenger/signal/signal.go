package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/julienrbrt/talktothem/internal/messenger"
)

type Client struct {
	phoneNumber string
	binaryPath  string
	dataPath    string
	connected   bool
	mu          sync.RWMutex

	messageHandler  func(messenger.Message)
	reactionHandler func(messenger.Message)
}

type Option func(*Client)

func WithBinaryPath(path string) Option {
	return func(c *Client) {
		c.binaryPath = path
	}
}

func WithDataPath(path string) Option {
	return func(c *Client) {
		c.dataPath = path
	}
}

func New(phoneNumber string, opts ...Option) *Client {
	c := &Client{
		phoneNumber: phoneNumber,
		binaryPath:  "signal-cli",
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	args := []string{"-a", c.phoneNumber, "receive", "--timeout", "3600"}
	if c.dataPath != "" {
		args = append([]string{"--config", c.dataPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start signal-cli: %w", err)
	}

	c.connected = true

	go c.processOutput(stdout)

	return nil
}

func (c *Client) processOutput(stdout io.Reader) {
	decoder := json.NewDecoder(stdout)
	for {
		var envelope map[string]any
		if err := decoder.Decode(&envelope); err != nil {
			continue
		}

		msg := c.parseEnvelope(envelope)
		if msg != nil && !msg.IsFromMe {
			if c.messageHandler != nil {
				c.messageHandler(*msg)
			}
		}
	}
}

func (c *Client) parseEnvelope(envelope map[string]any) *messenger.Message {
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil
	}

	msg := &messenger.Message{
		ContactID: getString(envelope, "source"),
		Timestamp: time.Unix(0, getInt64(data, "timestamp")*int64(time.Millisecond)),
		IsFromMe:  getBool(data, "isDelivery"),
	}

	if text, ok := data["message"].(string); ok {
		msg.Content = text
		msg.Type = messenger.TypeText
	}

	if reaction, ok := data["reaction"].(map[string]any); ok {
		msg.Type = messenger.TypeReaction
		msg.Reaction = getString(reaction, "emoji")
	}

	if attachments, ok := data["attachments"].([]any); ok && len(attachments) > 0 {
		msg.Type = messenger.TypeImage
		if att, ok := attachments[0].(map[string]any); ok {
			msg.MediaURL = getString(att, "filename")
		}
	}

	return msg
}

func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	return nil
}

func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *Client) GetContacts(ctx context.Context) ([]messenger.Contact, error) {
	args := []string{"-a", c.phoneNumber, "listContacts", "--json"}
	if c.dataPath != "" {
		args = append([]string{"--config", c.dataPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list contacts: %w", err)
	}

	var contacts []messenger.Contact
	lines := strings.SplitSeq(string(output), "\n")
	for line := range lines {
		if line == "" {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		contacts = append(contacts, messenger.Contact{
			ID:    getString(entry, "number"),
			Name:  getString(entry, "name"),
			Phone: getString(entry, "number"),
		})
	}

	return contacts, nil
}

func (c *Client) GetConversation(ctx context.Context, contactID string, limit int) ([]messenger.Message, error) {
	args := []string{"-a", c.phoneNumber, "listMessages", contactID, "--json"}
	if c.dataPath != "" {
		args = append([]string{"--config", c.dataPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}

	var messages []messenger.Message
	lines := strings.SplitSeq(string(output), "\n")
	for line := range lines {
		if line == "" {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		msg := messenger.Message{
			ID:        getString(entry, "timestamp"),
			ContactID: contactID,
			Content:   getString(entry, "message"),
			Timestamp: time.Unix(0, getInt64(entry, "timestamp")*int64(time.Millisecond)),
			IsFromMe:  getBool(entry, "isSentByMe"),
		}

		if msg.Content != "" {
			msg.Type = messenger.TypeText
		}

		messages = append(messages, msg)
	}

	if limit > 0 && len(messages) > limit {
		messages = messages[:limit]
	}

	return messages, nil
}

func (c *Client) SendMessage(ctx context.Context, contactID, content string) error {
	args := []string{"-a", c.phoneNumber, "send", contactID, "-m", content}
	if c.dataPath != "" {
		args = append([]string{"--config", c.dataPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to send message: %w: %s", err, string(output))
	}

	return nil
}

func (c *Client) SendReaction(ctx context.Context, contactID, messageID, emoji string) error {
	args := []string{
		"-a", c.phoneNumber,
		"sendReaction",
		contactID,
		"--emoji", emoji,
		"--target-timestamp", messageID,
	}
	if c.dataPath != "" {
		args = append([]string{"--config", c.dataPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to send reaction: %w: %s", err, string(output))
	}

	return nil
}

func (c *Client) OnMessage(handler func(messenger.Message)) {
	c.messageHandler = handler
}

func (c *Client) OnReaction(handler func(messenger.Message)) {
	c.reactionHandler = handler
}

func (c *Client) DownloadAttachment(ctx context.Context, attachmentID, outputPath string) error {
	args := []string{"-a", c.phoneNumber, "listAttachments", "--output", outputPath, attachmentID}
	if c.dataPath != "" {
		args = append([]string{"--config", c.dataPath}, args...)
	}

	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to download attachment: %w: %s", err, string(output))
	}

	return nil
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

func getBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}
