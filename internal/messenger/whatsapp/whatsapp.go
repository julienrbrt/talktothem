package whatsapp

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

	"github.com/julienrbrt/talktothem/internal/messenger"
)

var _ messenger.Messenger = &Client{}

const deviceID = "talktothem"

type Client struct {
	baseURL   string
	dataDir   string
	connected bool
	mu        sync.RWMutex
	number    string
	http      *http.Client

	messageHandler  func(messenger.Message)
	reactionHandler func(messenger.Message)
}

func New(dataDir string, baseURL string) (*Client, error) {
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}

	client := &Client{
		baseURL: baseURL,
		dataDir: dataDir,
		http:    &http.Client{Timeout: 30 * time.Second},
	}

	client.ensureDevice()

	if linked, number, _ := client.IsLinked(context.Background()); linked {
		client.number = number
		slog.Info("WhatsApp initialized with existing session", "number", number)
	}

	return client, nil
}

func (c *Client) Name() string {
	return "whatsapp"
}

func (c *Client) ensureDevice() error {
	existing, err := c.listDevices()
	if err != nil {
		slog.Warn("WhatsApp failed to list devices, will try to register", "error", err)
	} else {
		for _, d := range existing {
			if d.ID == deviceID {
				slog.Info("WhatsApp device already registered", "deviceID", deviceID)
				return nil
			}
		}
	}

	reqBody, _ := json.Marshal(map[string]string{"device_id": deviceID})
	req, err := c.newRequest(http.MethodPost, "/devices", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("register device: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register device failed (%d): %s", resp.StatusCode, string(body))
	}

	slog.Info("WhatsApp device registered", "deviceID", deviceID)
	return nil
}

func (c *Client) listDevices() ([]struct {
	ID   string `json:"id"`
	Name string `json:"display_name"`
}, error) {
	req, err := c.newRequest(http.MethodGet, "/devices", nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list devices status %d", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			ID   string `json:"id"`
			Name string `json:"display_name"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Results, nil
}

func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Device-Id", deviceID)
	return req, nil
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	linked, number, err := c.IsLinked(ctx)
	if err != nil {
		return fmt.Errorf("check link status: %w", err)
	}
	if !linked {
		return fmt.Errorf("not linked")
	}

	c.number = number

	req, err := c.newRequest(http.MethodGet, "/app/reconnect", nil)
	if err != nil {
		return fmt.Errorf("reconnect request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reconnect: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("WhatsApp reconnect returned", "status", resp.StatusCode)
	}

	c.connected = true
	slog.Info("WhatsApp connected", "number", c.number)
	return nil
}

func (c *Client) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = false
	slog.Info("WhatsApp disconnected")
	return nil
}

func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *Client) StartLinking(ctx context.Context, deviceName string) ([]byte, error) {
	slog.Info("Starting WhatsApp linking...")

	if err := c.ensureDevice(); err != nil {
		return nil, fmt.Errorf("ensure device: %w", err)
	}

	linked, _, _ := c.IsLinked(ctx)
	if linked {
		logoutReq, _ := c.newRequest(http.MethodGet, "/app/logout", nil)
		if logoutReq != nil {
			logoutResp, err := c.http.Do(logoutReq)
			if err == nil {
				logoutResp.Body.Close()
				slog.Info("WhatsApp cleared previous session before login")
			}
		}
		if err := c.ensureDevice(); err != nil {
			return nil, fmt.Errorf("re-create device after logout: %w", err)
		}
	}

	req, err := c.newRequest(http.MethodGet, "/app/login", nil)
	if err != nil {
		return nil, fmt.Errorf("login request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("login failed (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Results struct {
			QRLink     string `json:"qr_link"`
			QRDuration int    `json:"qr_duration"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if result.Code != "SUCCESS" && result.Code != "200" {
		return nil, fmt.Errorf("login failed: %s", result.Message)
	}

	qrLink := result.Results.QRLink
	if qrLink == "" {
		return nil, fmt.Errorf("no qr_link in response")
	}

	qrLink = strings.TrimPrefix(qrLink, "http://localhost:3000")
	if !strings.HasPrefix(qrLink, "http") {
		qrLink = c.baseURL + qrLink
	}

	slog.Info("WhatsApp QR code generated, downloading image", "link", qrLink)

	imgReq, err := c.newRequest(http.MethodGet, strings.TrimPrefix(qrLink, c.baseURL), nil)
	if err != nil {
		return nil, fmt.Errorf("image request: %w", err)
	}

	imgResp, err := c.http.Do(imgReq)
	if err != nil {
		return nil, fmt.Errorf("download qr image: %w", err)
	}
	defer imgResp.Body.Close()

	if imgResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download qr image: status %d", imgResp.StatusCode)
	}

	imgData, err := io.ReadAll(imgResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read qr image: %w", err)
	}

	return imgData, nil
}

func (c *Client) IsLinked(ctx context.Context) (bool, string, error) {
	req, err := c.newRequest(http.MethodGet, "/app/status", nil)
	if err != nil {
		return false, "", nil
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, "", nil
	}

	var result struct {
		Results struct {
			IsConnected bool   `json:"is_connected"`
			IsLoggedIn  bool   `json:"is_logged_in"`
			DeviceID    string `json:"device_id"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, "", nil
	}

	if result.Results.IsConnected && result.Results.IsLoggedIn {
		number := result.Results.DeviceID
		if number != "" {
			number = strings.TrimSuffix(number, "@s.whatsapp.net")
		}
		return true, number, nil
	}

	return false, "", nil
}

func (c *Client) GetOwnProfile(ctx context.Context) (*messenger.OwnProfile, error) {
	phone := c.number
	if phone != "" && !strings.Contains(phone, "@") {
		phone = phone + "@s.whatsapp.net"
	}

	endpoint := fmt.Sprintf("/user/info?phone=%s", url.QueryEscape(phone))

	req, err := c.newRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("get own profile: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get own profile: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get own profile failed (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Results struct {
			VerifiedName string `json:"verified_name"`
			Status       string `json:"status"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode own profile: %w", err)
	}

	return &messenger.OwnProfile{
		Name:  result.Results.VerifiedName,
		About: result.Results.Status,
	}, nil
}

func (c *Client) GetContacts(ctx context.Context) ([]messenger.Contact, error) {
	slog.Info("WhatsApp getting contacts...")

	req, err := c.newRequest(http.MethodGet, "/user/my/contacts", nil)
	if err != nil {
		return nil, fmt.Errorf("get contacts: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get contacts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get contacts failed (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Results struct {
			Data []struct {
				JID  string `json:"jid"`
				Name string `json:"name"`
			} `json:"data"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode contacts: %w", err)
	}

	var contacts []messenger.Contact
	for _, c := range result.Results.Data {
		phone := strings.TrimSuffix(c.JID, "@s.whatsapp.net")
		if phone == "" {
			continue
		}
		name := c.Name
		if name == "" {
			name = phone
		}
		contacts = append(contacts, messenger.Contact{
			ID:    phone,
			Name:  name,
			Phone: phone,
		})
	}

	slog.Info("WhatsApp contacts retrieved", "count", len(contacts))
	return contacts, nil
}

func (c *Client) GetConversation(ctx context.Context, contactID string, limit int) ([]messenger.Message, error) {
	if !c.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}

	chatJID := contactID
	if !strings.Contains(chatJID, "@") {
		chatJID = contactID + "@s.whatsapp.net"
	}

	req, err := c.newRequest(http.MethodGet, fmt.Sprintf("/chat/%s/messages?limit=%d", chatJID, limit), nil)
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get conversation failed (%d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Results struct {
			Data []struct {
				ID        string    `json:"id"`
				ChatJID   string    `json:"chat_jid"`
				SenderJID string    `json:"sender_jid"`
				Content   string    `json:"content"`
				Timestamp time.Time `json:"timestamp"`
				IsFromMe  bool      `json:"is_from_me"`
				MediaType string    `json:"media_type"`
				URL       string    `json:"url"`
			} `json:"data"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}

	var messages []messenger.Message
	for _, m := range result.Results.Data {
		contactPhone := strings.TrimSuffix(m.ChatJID, "@s.whatsapp.net")
		senderPhone := strings.TrimSuffix(m.SenderJID, "@s.whatsapp.net")

		msg := messenger.Message{
			ID:        m.ID,
			ContactID: contactPhone,
			Content:   m.Content,
			Timestamp: m.Timestamp,
			IsFromMe:  m.IsFromMe,
		}

		if m.MediaType != "" {
			msg.Type = messenger.MessageType(m.MediaType)
			if m.URL != "" {
				msg.MediaURLs = []string{m.URL}
			}
		}

		if strings.Contains(m.ChatJID, "@g.us") {
			msg.IsGroup = true
			msg.ContactID = senderPhone
		}

		if msg.Content != "" || len(msg.MediaURLs) > 0 {
			messages = append(messages, msg)
		}
	}

	return messages, nil
}

func (c *Client) SendMessage(ctx context.Context, contactID, content string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected")
	}

	phone := contactID
	if !strings.Contains(phone, "@") {
		phone = phone + "@s.whatsapp.net"
	}

	body, _ := json.Marshal(map[string]string{
		"phone":   phone,
		"message": content,
	})

	req, err := c.newRequest(http.MethodPost, "/send/message", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send message failed (%d): %s", resp.StatusCode, string(respBody))
	}

	slog.Info("WhatsApp message sent", "contactID", contactID)
	return nil
}

func (c *Client) SendReaction(ctx context.Context, contactID, messageID, emoji string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected")
	}

	phone := contactID
	if !strings.Contains(phone, "@") {
		phone = phone + "@s.whatsapp.net"
	}

	body, _ := json.Marshal(map[string]string{
		"phone": phone,
		"emoji": emoji,
	})

	req, err := c.newRequest(http.MethodPost, fmt.Sprintf("/message/%s/reaction", messageID), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send reaction: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send reaction: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send reaction failed (%d): %s", resp.StatusCode, string(respBody))
	}

	slog.Info("WhatsApp reaction sent", "contactID", contactID, "emoji", emoji)
	return nil
}

func (c *Client) MarkRead(ctx context.Context, contactID string, messageIDs []string) error {
	if !c.IsConnected() || len(messageIDs) == 0 {
		return nil
	}

	phone := contactID
	if !strings.Contains(phone, "@") {
		phone = phone + "@s.whatsapp.net"
	}

	body, _ := json.Marshal(map[string]string{
		"phone": phone,
	})

	for _, msgID := range messageIDs {
		req, err := c.newRequest(http.MethodPost, fmt.Sprintf("/message/%s/read", msgID), bytes.NewReader(body))
		if err != nil {
			slog.Warn("WhatsApp mark read failed", "messageID", msgID, "error", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			slog.Warn("WhatsApp mark read failed", "messageID", msgID, "error", err)
			continue
		}
		resp.Body.Close()
	}

	slog.Info("WhatsApp messages marked as read", "contactID", contactID, "count", len(messageIDs))
	return nil
}

func (c *Client) SendTypingIndicator(ctx context.Context, contactID string, show bool) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected")
	}

	phone := contactID
	if !strings.Contains(phone, "@") {
		phone = phone + "@s.whatsapp.net"
	}

	action := "stop"
	if show {
		action = "start"
	}

	body, _ := json.Marshal(map[string]string{
		"phone":  phone,
		"action": action,
	})

	req, err := c.newRequest(http.MethodPost, "/send/chat-presence", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send typing indicator: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("send typing indicator: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send typing indicator failed (%d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *Client) OnMessage(handler func(messenger.Message)) {
	c.messageHandler = handler
}

func (c *Client) OnReaction(handler func(messenger.Message)) {
	c.reactionHandler = handler
}

func (c *Client) StartReceiving(ctx context.Context) {
	slog.Info("WhatsApp receiving started (polling mode)")

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.pollMessages(ctx)
			}
		}
	}()
}

func (c *Client) pollMessages(ctx context.Context) {
	c.mu.RLock()
	if !c.connected || c.messageHandler == nil {
		c.mu.RUnlock()
		return
	}
	c.mu.RUnlock()

	req, err := c.newRequest(http.MethodGet, "/chats?limit=10", nil)
	if err != nil {
		return
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var result struct {
		Results struct {
			Data []struct {
				JID           string `json:"jid"`
				Name          string `json:"name"`
				LastMessageAt string `json:"last_message_time"`
			} `json:"data"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	for _, chat := range result.Results.Data {
		if strings.Contains(chat.JID, "@g.us") {
			continue
		}

		contactPhone := strings.TrimSuffix(chat.JID, "@s.whatsapp.net")
		if contactPhone == "" {
			continue
		}

		msgs, err := c.GetConversation(ctx, contactPhone, 1)
		if err != nil || len(msgs) == 0 {
			continue
		}

		latest := msgs[0]
		if !latest.IsFromMe && latest.Content != "" {
			c.messageHandler(latest)
		}
	}
}
