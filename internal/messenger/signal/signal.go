package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

var _ messenger.Messenger = &Client{}

type Client struct {
	number    string
	baseURL   string
	wsURL     string
	client    *http.Client
	connected bool
	mu        sync.RWMutex

	messageHandler  func(messenger.Message)
	reactionHandler func(messenger.Message)
}

func (c *Client) Name() string {
	return "signal"
}

func New(number, baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	return &Client{
		number:  number,
		baseURL: baseURL,
		wsURL:   wsURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func NewWithoutNumber(baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8081"
	}
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	return &Client{
		baseURL: baseURL,
		wsURL:   wsURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) SetNumber(number string) {
	c.number = number
}

func (c *Client) GetNumber() string {
	return c.number
}

func (c *Client) GetBaseURL() string {
	return c.baseURL
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()

	return nil
}

func (c *Client) StartReceiving(ctx context.Context) {
	go c.receiveLoop(ctx)
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

func (c *Client) StartLinking(ctx context.Context, deviceName string) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/v1/qrcodelink?device_name=%s", c.baseURL, url.QueryEscape(deviceName))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get qr code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get qr code failed: %s", string(body))
	}

	return io.ReadAll(resp.Body)
}

func (c *Client) GetLinkingURI(ctx context.Context, deviceName string) (string, error) {
	endpoint := fmt.Sprintf("%s/v1/qrcodelink/raw?device_name=%s", c.baseURL, url.QueryEscape(deviceName))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get linking uri: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get linking uri failed: %s", string(body))
	}

	var result struct {
		DeviceLinkURI string `json:"device_link_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.DeviceLinkURI, nil
}

func (c *Client) ListAccounts(ctx context.Context) ([]string, error) {
	endpoint := fmt.Sprintf("%s/v1/accounts", c.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list accounts failed: %s", string(body))
	}

	var accounts []string
	if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return accounts, nil
}

func (c *Client) IsLinked(ctx context.Context) (bool, string, error) {
	accounts, err := c.ListAccounts(ctx)
	if err != nil {
		return false, "", err
	}

	if len(accounts) > 0 {
		c.mu.Lock()
		c.number = accounts[0]
		c.mu.Unlock()
		return true, accounts[0], nil
	}

	return false, "", nil
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
		"recipient":     contactID,
		"reaction":      emoji,
		"timestamp":     time.Now().UnixMilli(),
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

func (c *Client) MarkRead(ctx context.Context, contactID string, messageIDs []string) error {
	if len(messageIDs) == 0 {
		return nil
	}

	var lastTimestamp int64
	for _, id := range messageIDs {
		var ts int64
		if _, err := fmt.Sscanf(id, "%d", &ts); err == nil && ts > lastTimestamp {
			lastTimestamp = ts
		}
	}

	if lastTimestamp == 0 {
		return nil
	}

	endpoint := fmt.Sprintf("%s/v1/receipts/%s", c.baseURL, url.PathEscape(c.number))

	payload := map[string]any{
		"recipient":    contactID,
		"receipt_type": "read",
		"timestamp":    lastTimestamp,
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
		return fmt.Errorf("send receipts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send receipts failed: %s", string(respBody))
	}

	return nil
}

func (c *Client) SendTypingIndicator(ctx context.Context, contactID string, show bool) error {
	endpoint := fmt.Sprintf("%s/v1/typing-indicator/%s", c.baseURL, url.PathEscape(c.number))

	payload := map[string]any{
		"recipient": contactID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	var method string
	if show {
		method = http.MethodPut
	} else {
		method = http.MethodDelete
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send typing indicator: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send typing indicator failed: %s", string(respBody))
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
	dialer := websocket.DefaultDialer

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !c.IsConnected() {
			slog.Info("Signal Not connected, stopping receive loop")
			return
		}

		c.mu.RLock()
		number := c.number
		c.mu.RUnlock()

		if number == "" {
			slog.Debug("Signal messenger has no number set, checking link status...")
			linked, _, err := c.IsLinked(ctx)
			if err != nil || !linked {
				time.Sleep(10 * time.Second)
			}
			continue
		}

		endpoint := fmt.Sprintf("%s/v1/receive/%s", c.wsURL, url.PathEscape(number))
		slog.Info("Connecting to Signal WebSocket", "endpoint", endpoint)

		conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
		if err != nil {
			status := "unknown"
			if resp != nil {
				status = resp.Status
			}
			slog.Warn("Signal WebSocket dial error, retrying in 5s...", "error", err, "status", status)
			time.Sleep(5 * time.Second)
			continue
		}
		slog.Info("Signal WebSocket connected, waiting for messages...")

		// Set up ping handler to keep connection alive
		conn.SetPingHandler(func(appData string) error {
			slog.Debug("Signal Received ping, sending pong")
			return conn.WriteMessage(websocket.PongMessage, nil)
		})

		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			default:
			}

			// Set read deadline to detect stale connections
			if err := conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
				slog.Error("Signal Failed to set read deadline", "error", err)
				conn.Close()
				break
			}

			messageType, raw, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					slog.Info("Signal WebSocket closed normally", "error", err)
				} else if err.Error() == "read timeout" || strings.Contains(err.Error(), "timeout") {
					slog.Debug("Signal Read timeout, reconnecting...")
				} else {
					slog.Error("Signal WebSocket read error", "error", err)
				}
				conn.Close()
				time.Sleep(2 * time.Second)
				break
			}

			slog.Debug("Signal Received WebSocket message", "type", messageType, "bytes", len(raw))
			slog.Debug("Signal Raw content", "content", string(raw))

			var rawMessages []json.RawMessage
			if err := json.Unmarshal(raw, &rawMessages); err != nil {
				// Maybe it's a single message, not an array
				var singleMsg json.RawMessage
				if err2 := json.Unmarshal(raw, &singleMsg); err2 == nil {
					rawMessages = []json.RawMessage{singleMsg}
				} else {
					slog.Error("Signal Failed to unmarshal messages", "error", err)
					continue
				}
			}

			slog.Debug("Signal Parsed message envelopes", "count", len(rawMessages))

			for _, raw := range rawMessages {
				msg := c.parseMessage(raw, "")
				if msg == nil {
					// Try unwrapping from "envelope" key (json-rpc mode format)
					var wrapped struct {
						Envelope json.RawMessage `json:"envelope"`
					}
					if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Envelope != nil {
						msg = c.parseMessage(wrapped.Envelope, "")
					}
				}
				if msg == nil {
					slog.Debug("Signal Parsed nil message, skipping")
					continue
				}
				if msg.Content == "" && msg.Type != messenger.TypeReaction {
					slog.Debug("Signal Empty content, skipping (likely a receipt)")
					continue
				}

				slog.Info("Signal Received message", "contactID", msg.ContactID, "content", msg.Content)

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
}

func (c *Client) parseMessage(raw json.RawMessage, filterContact string) *messenger.Message {
	var envelope struct {
		Account    string `json:"account"`
		Source     string `json:"source"`
		SourceName string `json:"sourceName"`
		Timestamp  int64  `json:"timestamp"`
		Type       string `json:"type"`

		DataMessage *struct {
			Message          string `json:"message"`
			Timestamp        int64  `json:"timestamp"`
			ExpiresInSeconds int    `json:"expiresInSeconds"`
			Attachments      []struct {
				ContentType string `json:"contentType"`
				Filename    string `json:"filename"`
			} `json:"attachments"`
			Sticker *struct {
				PackID    string `json:"packId"`
				StickerID int    `json:"stickerId"`
				Filename  string `json:"filename"`
			} `json:"sticker"`
		} `json:"dataMessage"`

		SyncMessage *struct {
			SentMessage *struct {
				Destination string `json:"destination"`
				Message     string `json:"message"`
				Timestamp   int64  `json:"timestamp"`
			} `json:"sentMessage"`
		} `json:"syncMessage"`

		Reaction *struct {
			Emoji               string `json:"emoji"`
			TargetAuthor        string `json:"targetAuthor"`
			TargetSentTimestamp int64  `json:"targetSentTimestamp"`
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
		ID:        fmt.Sprintf("%d", envelope.Timestamp),
	}

	switch {
	case envelope.SyncMessage != nil && envelope.SyncMessage.SentMessage != nil:
		sent := envelope.SyncMessage.SentMessage
		msg.ContactID = sent.Destination
		msg.Content = sent.Message
		msg.IsFromMe = true
		msg.Timestamp = time.UnixMilli(sent.Timestamp)
		msg.ID = fmt.Sprintf("%d", sent.Timestamp)

	case envelope.DataMessage != nil:
		msg.ContactID = envelope.Source
		msg.Content = envelope.DataMessage.Message
		msg.Timestamp = time.UnixMilli(envelope.DataMessage.Timestamp)
		msg.ID = fmt.Sprintf("%d", envelope.DataMessage.Timestamp)
		msg.Type = messenger.TypeText

		if len(envelope.DataMessage.Attachments) > 0 {
			msg.Type = messenger.TypeImage
			for _, att := range envelope.DataMessage.Attachments {
				msg.MediaURLs = append(msg.MediaURLs, att.Filename)
			}
		}

		if envelope.DataMessage.Sticker != nil {
			msg.Type = messenger.TypeSticker
			msg.MediaURLs = append(msg.MediaURLs, envelope.DataMessage.Sticker.Filename)
		}

	case envelope.Reaction != nil:
		msg.ContactID = envelope.Source
		msg.Type = messenger.TypeReaction
		msg.Reaction = envelope.Reaction.Emoji

	default:
		return nil
	}

	// Detect if message is from a group
	// Group IDs in Signal don't start with + (which is for phone numbers)
	// or contain special characters/patterns indicating a group
	if msg.ContactID != "" {
		// If contact ID doesn't start with '+', it's likely a group ID
		// Group IDs in Signal are typically in the format: group.<uuid>
		msg.IsGroup = !strings.HasPrefix(msg.ContactID, "+")
	}

	if filterContact != "" && msg.ContactID != filterContact {
		return nil
	}

	return msg
}
