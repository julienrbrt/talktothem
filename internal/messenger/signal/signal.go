package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/julienrbrt/talktothem/internal/messenger"
)

type Client struct {
	number    string
	baseURL   string
	client    *http.Client
	connected bool
	mu        sync.RWMutex

	messageHandler  func(messenger.Message)
	reactionHandler func(messenger.Message)
}

func New(number, baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	return &Client{
		number:  number,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()

	go c.receiveLoop(ctx)
	return nil
}

func (c *Client) Disconnect() error {
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
	return nil
}

func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *Client) GetContacts(ctx context.Context) ([]messenger.Contact, error) {
	endpoint := fmt.Sprintf("%s/v1/contacts/%s", c.baseURL, url.PathEscape(c.number))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get contacts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get contacts failed: %s", string(body))
	}

	var contacts []struct {
		Number string `json:"number"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&contacts); err != nil {
		return nil, fmt.Errorf("decode contacts: %w", err)
	}

	result := make([]messenger.Contact, len(contacts))
	for i, contact := range contacts {
		result[i] = messenger.Contact{
			ID:    contact.Number,
			Phone: contact.Number,
			Name:  contact.Name,
		}
	}
	return result, nil
}

func (c *Client) GetConversation(ctx context.Context, contactID string, limit int) ([]messenger.Message, error) {
	endpoint := fmt.Sprintf("%s/v1/receive/%s?timeout=1", c.baseURL, url.PathEscape(c.number))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get messages failed: %s", string(body))
	}

	var rawMessages []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rawMessages); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, fmt.Errorf("decode messages: %w", err)
	}

	var messages []messenger.Message
	for _, raw := range rawMessages {
		msg := c.parseMessage(raw, contactID)
		if msg == nil {
			continue
		}
		messages = append(messages, *msg)
	}

	if limit > 0 && len(messages) > limit {
		messages = messages[:limit]
	}

	return messages, nil
}

func (c *Client) SendMessage(ctx context.Context, contactID, content string) error {
	endpoint := fmt.Sprintf("%s/v2/send", c.baseURL)

	payload := map[string]any{
		"message":    content,
		"number":     c.number,
		"recipients": []string{contactID},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send message failed: %s", string(respBody))
	}

	return nil
}

func (c *Client) SendReaction(ctx context.Context, contactID, messageID, emoji string) error {
	endpoint := fmt.Sprintf("%s/v1/reactions/%s", c.baseURL, url.PathEscape(c.number))

	payload := map[string]any{
		"recipient":    contactID,
		"reaction":     emoji,
		"timestamp":    time.Now().UnixMilli(),
		"target_author": contactID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send reaction: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send reaction failed: %s", string(respBody))
	}

	return nil
}

func (c *Client) OnMessage(handler func(messenger.Message)) {
	c.messageHandler = handler
}

func (c *Client) OnReaction(handler func(messenger.Message)) {
	c.reactionHandler = handler
}

func (c *Client) receiveLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !c.IsConnected() {
			return
		}

		endpoint := fmt.Sprintf("%s/v1/receive/%s?timeout=60", c.baseURL, url.PathEscape(c.number))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		resp, err := c.client.Do(req)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			time.Sleep(5 * time.Second)
			continue
		}

		var rawMessages []json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&rawMessages); err != nil {
			resp.Body.Close()
			if err == io.EOF {
				continue
			}
			time.Sleep(1 * time.Second)
			continue
		}
		resp.Body.Close()

		for _, raw := range rawMessages {
			msg := c.parseMessage(raw, "")
			if msg == nil || msg.IsFromMe {
				continue
			}

			if msg.Type == messenger.TypeReaction {
				if c.reactionHandler != nil {
					c.reactionHandler(*msg)
				}
			} else {
				if c.messageHandler != nil {
					c.messageHandler(*msg)
				}
			}
		}
	}
}

func (c *Client) parseMessage(raw json.RawMessage, filterContact string) *messenger.Message {
	var envelope struct {
		Account    string `json:"account"`
		Source     string `json:"source"`
		SourceName string `json:"sourceName"`
		Timestamp  int64  `json:"timestamp"`
		Type       string `json:"type"`

		DataMessage *struct {
			Message     string `json:"message"`
			Timestamp   int64  `json:"timestamp"`
			ExpiresInSeconds int `json:"expiresInSeconds"`
			Attachments []struct {
				ContentType string `json:"contentType"`
				Filename    string `json:"filename"`
			} `json:"attachments"`
		} `json:"dataMessage"`

		SyncMessage *struct {
			SentMessage *struct {
				Destination string `json:"destination"`
				Message     string `json:"message"`
				Timestamp   int64  `json:"timestamp"`
			} `json:"sentMessage"`
		} `json:"syncMessage"`

		Reaction *struct {
			Emoji            string `json:"emoji"`
			TargetAuthor     string `json:"targetAuthor"`
			TargetSentTimestamp int64 `json:"targetSentTimestamp"`
		} `json:"reaction"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}

	if envelope.Source == "" && (envelope.SyncMessage == nil || envelope.SyncMessage.SentMessage == nil) {
		return nil
	}

	msg := &messenger.Message{
		Timestamp: time.UnixMilli(envelope.Timestamp),
	}

	switch {
	case envelope.SyncMessage != nil && envelope.SyncMessage.SentMessage != nil:
		sent := envelope.SyncMessage.SentMessage
		msg.ContactID = sent.Destination
		msg.Content = sent.Message
		msg.IsFromMe = true
		msg.Timestamp = time.UnixMilli(sent.Timestamp)

	case envelope.DataMessage != nil:
		msg.ContactID = envelope.Source
		msg.Content = envelope.DataMessage.Message
		msg.Timestamp = time.UnixMilli(envelope.DataMessage.Timestamp)
		msg.Type = messenger.TypeText

		if len(envelope.DataMessage.Attachments) > 0 {
			msg.Type = messenger.TypeImage
			msg.MediaURL = envelope.DataMessage.Attachments[0].Filename
		}

	case envelope.Reaction != nil:
		msg.ContactID = envelope.Source
		msg.Type = messenger.TypeReaction
		msg.Reaction = envelope.Reaction.Emoji

	default:
		return nil
	}

	if filterContact != "" && msg.ContactID != filterContact {
		return nil
	}

	return msg
}
