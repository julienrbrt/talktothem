package messenger

import (
	"context"
	"time"
)

type MessageType string

const (
	TypeText     MessageType = "text"
	TypeImage    MessageType = "image"
	TypeReaction MessageType = "reaction"
)

type Message struct {
	ID        string
	ContactID string
	Content   string
	Type      MessageType
	MediaURL  string
	Timestamp time.Time
	IsFromMe  bool
	Reaction  string
}

type Contact struct {
	ID      string
	Name    string
	Phone   string
	Enabled bool
}

type Messenger interface {
	Name() string
	Connect(ctx context.Context) error
	Disconnect() error
	IsConnected() bool

	StartLinking(ctx context.Context, deviceName string) ([]byte, error)
	IsLinked(ctx context.Context) (bool, string, error)

	GetContacts(ctx context.Context) ([]Contact, error)
	GetConversation(ctx context.Context, contactID string, limit int) ([]Message, error)
	SendMessage(ctx context.Context, contactID, content string) error
	SendReaction(ctx context.Context, contactID, messageID, emoji string) error

	OnMessage(handler func(Message))
	OnReaction(handler func(Message))
	StartReceiving(ctx context.Context)
}
