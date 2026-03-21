package conversation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/messenger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = db.Init(tmpDir)
	require.NoError(t, err)

	h, err := NewHistory(tmpDir, "test-contact")
	require.NoError(t, err)

	now := time.Now()

	t.Run("add message", func(t *testing.T) {
		msg := messenger.Message{
			ID:        "1",
			ContactID: "test-contact",
			Content:   "Hello",
			Type:      messenger.TypeText,
			Timestamp: now,
			IsFromMe:  false,
		}
		err := h.Add(msg)
		require.NoError(t, err)

		recent := h.GetRecent(10)
		assert.Len(t, recent, 1)
		assert.Equal(t, "Hello", recent[0].Content)
	})

	t.Run("add multiple messages", func(t *testing.T) {
		msg := messenger.Message{
			ID:        "2",
			ContactID: "test-contact",
			Content:   "Hi there",
			Type:      messenger.TypeText,
			Timestamp: now.Add(time.Minute),
			IsFromMe:  true,
		}
		err := h.Add(msg)
		require.NoError(t, err)

		recent := h.GetRecent(10)
		assert.Len(t, recent, 2)
	})

	t.Run("get recent with limit", func(t *testing.T) {
		recent := h.GetRecent(1)
		assert.Len(t, recent, 1)
		assert.Equal(t, "Hi there", recent[0].Content)
	})

	t.Run("get since", func(t *testing.T) {
		since := now.Add(30 * time.Second)
		msgs := h.GetSince(since)
		assert.Len(t, msgs, 1)
	})
}

func TestHistoryPersistence(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = db.Init(tmpDir)
	require.NoError(t, err)

	h1, err := NewHistory(tmpDir, "persist-test")
	require.NoError(t, err)

	msg := messenger.Message{
		ID:        "1",
		ContactID: "persist-test",
		Content:   "Test message",
		Type:      messenger.TypeText,
		Timestamp: time.Now(),
		IsFromMe:  false,
	}
	err = h1.Add(msg)
	require.NoError(t, err)

	h2, err := NewHistory(tmpDir, "persist-test")
	require.NoError(t, err)

	recent := h2.GetRecent(10)
	assert.Len(t, recent, 1)
	assert.Equal(t, "Test message", recent[0].Content)
}

type mockMessenger struct {
	messages []messenger.Message
}

func (m *mockMessenger) Name() string                    { return "mock" }
func (m *mockMessenger) Connect(_ context.Context) error { return nil }
func (m *mockMessenger) Disconnect() error               { return nil }
func (m *mockMessenger) IsConnected() bool               { return true }
func (m *mockMessenger) StartLinking(ctx context.Context, deviceName string) ([]byte, error) {
	return nil, nil
}
func (m *mockMessenger) IsLinked(ctx context.Context) (bool, string, error) { return true, "mock", nil }
func (m *mockMessenger) GetOwnProfile(_ context.Context) (*messenger.OwnProfile, error) {
	return nil, nil
}
func (m *mockMessenger) GetContacts(_ context.Context) ([]messenger.Contact, error) {
	return nil, nil
}
func (m *mockMessenger) GetConversation(_ context.Context, _ string, _ int) ([]messenger.Message, error) {
	return m.messages, nil
}
func (m *mockMessenger) SendMessage(_ context.Context, _, _ string) error { return nil }
func (m *mockMessenger) SendReaction(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *mockMessenger) MarkRead(_ context.Context, _ string, _ []string) error        { return nil }
func (m *mockMessenger) SendTypingIndicator(_ context.Context, _ string, _ bool) error { return nil }
func (m *mockMessenger) OnMessage(_ func(messenger.Message))                           {}
func (m *mockMessenger) OnReaction(_ func(messenger.Message))                          {}
func (m *mockMessenger) StartReceiving(_ context.Context)                              {}

func TestHistorySync(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = db.Init(tmpDir)
	require.NoError(t, err)

	now := time.Now()
	mock := &mockMessenger{
		messages: []messenger.Message{
			{ID: "1", ContactID: "sync-test", Content: "Msg 1", Timestamp: now},
			{ID: "2", ContactID: "sync-test", Content: "Msg 2", Timestamp: now.Add(time.Minute)},
		},
	}

	h, err := NewHistory(tmpDir, "sync-test")
	require.NoError(t, err)

	err = h.Sync(context.Background(), mock, "sync-test")
	require.NoError(t, err)

	recent := h.GetRecent(10)
	assert.Len(t, recent, 2)
}
