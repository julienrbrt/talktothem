package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

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

func New(number, baseURL string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:8080"
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
		baseURL = "http://localhost:8080"
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
	dialer := websocket.DefaultDialer

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !c.IsConnected() {
			fmt.Println("[Signal] Not connected, stopping receive loop")
			return
		}

		endpoint := fmt.Sprintf("%s/v1/receive/%s", c.wsURL, url.PathEscape(c.number))
		fmt.Printf("[Signal] Connecting to WebSocket: %s\n", endpoint)

		conn, _, err := dialer.DialContext(ctx, endpoint, nil)
		if err != nil {
			fmt.Printf("[Signal] WebSocket dial error: %v, retrying in 5s...\n", err)
			time.Sleep(5 * time.Second)
			continue
		}
		fmt.Println("[Signal] WebSocket connected, waiting for messages...")

		// Set up ping handler to keep connection alive
		conn.SetPingHandler(func(appData string) error {
			fmt.Println("[Signal] Received ping, sending pong")
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
				fmt.Printf("[Signal] Failed to set read deadline: %v\n", err)
				break
			}

			messageType, raw, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					fmt.Printf("[Signal] WebSocket closed normally: %v\n", err)
				} else if err.Error() == "read timeout" || strings.Contains(err.Error(), "timeout") {
					fmt.Println("[Signal] Read timeout, connection still alive, continuing...")
					continue
				} else {
					fmt.Printf("[Signal] WebSocket read error: %v\n", err)
				}
				conn.Close()
				time.Sleep(2 * time.Second)
				break
			}

			fmt.Printf("[Signal] Received WebSocket message type=%d, %d bytes\n", messageType, len(raw))
			fmt.Printf("[Signal] Raw content: %s\n", string(raw))

			var rawMessages []json.RawMessage
			if err := json.Unmarshal(raw, &rawMessages); err != nil {
				// Maybe it's a single message, not an array
				var singleMsg json.RawMessage
				if err2 := json.Unmarshal(raw, &singleMsg); err2 == nil {
					rawMessages = []json.RawMessage{singleMsg}
				} else {
					fmt.Printf("[Signal] Failed to unmarshal messages: %v\n", err)
					continue
				}
			}

			fmt.Printf("[Signal] Parsed %d message envelopes\n", len(rawMessages))

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
					fmt.Println("[Signal] Parsed nil message, skipping")
					continue
				}
				if msg.IsFromMe {
					fmt.Printf("[Signal] Message from me to %s, skipping\n", msg.ContactID)
					continue
				}
				if msg.Content == "" && msg.Type != messenger.TypeReaction {
					fmt.Println("[Signal] Empty content, skipping (likely a receipt)")
					continue
				}

				fmt.Printf("[Signal] Received message from %s: %s\n", msg.ContactID, msg.Content)

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
