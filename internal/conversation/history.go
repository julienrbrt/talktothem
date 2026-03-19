package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/julienrbrt/talktothem/internal/messenger"
)

type History struct {
	mu       sync.RWMutex
	messages []messenger.Message
	filePath string
}

type historyFile struct {
	Messages []messenger.Message `json:"messages"`
}

func NewHistory(dataPath, contactID string) (*History, error) {
	if err := os.MkdirAll(dataPath, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	filePath := filepath.Join(dataPath, contactID+".json")
	h := &History{
		filePath: filePath,
		messages: make([]messenger.Message, 0),
	}

	if err := h.load(); err != nil {
		return nil, err
	}

	return h, nil
}

func (h *History) load() error {
	data, err := os.ReadFile(h.filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read history file: %w", err)
	}

	var hf historyFile
	if err := json.Unmarshal(data, &hf); err != nil {
		return fmt.Errorf("failed to unmarshal history: %w", err)
	}

	h.messages = hf.Messages
	return nil
}

func (h *History) save() error {
	hf := historyFile{Messages: h.messages}
	data, err := json.MarshalIndent(hf, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal history: %w", err)
	}

	return os.WriteFile(h.filePath, data, 0o600)
}

func (h *History) Add(msg messenger.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.messages = append(h.messages, msg)
	sort.Slice(h.messages, func(i, j int) bool {
		return h.messages[i].Timestamp.Before(h.messages[j].Timestamp)
	})

	return h.save()
}

func (h *History) GetRecent(limit int) []messenger.Message {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if limit <= 0 || limit > len(h.messages) {
		return h.messages
	}

	start := len(h.messages) - limit
	return h.messages[start:]
}

func (h *History) GetSince(since time.Time) []messenger.Message {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []messenger.Message
	for _, msg := range h.messages {
		if msg.Timestamp.After(since) {
			result = append(result, msg)
		}
	}
	return result
}

func (h *History) Sync(ctx context.Context, m messenger.Messenger, contactID string) error {
	msgs, err := m.GetConversation(ctx, contactID, 0)
	if err != nil {
		return fmt.Errorf("failed to get conversation: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	existingIDs := make(map[string]bool)
	for _, msg := range h.messages {
		existingIDs[msg.ID] = true
	}

	for _, msg := range msgs {
		if !existingIDs[msg.ID] {
			h.messages = append(h.messages, msg)
		}
	}

	sort.Slice(h.messages, func(i, j int) bool {
		return h.messages[i].Timestamp.Before(h.messages[j].Timestamp)
	})

	return h.save()
}
