package conversation

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

type History struct {
	contactID string
}

func NewHistory(dataPath, contactID string) (*History, error) {
	return &History{contactID: contactID}, nil
}

func (h *History) Add(msg messenger.Message) error {
	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}

	dbMsg := db.Message{
		ID:        msg.ID,
		ContactID: h.contactID,
		Content:   msg.Content,
		Type:      string(msg.Type),
		MediaURL:  msg.MediaURL,
		Timestamp: msg.Timestamp.UnixMilli(),
		IsFromMe:  msg.IsFromMe,
		Reaction:  msg.Reaction,
	}

	return db.DB.Create(&dbMsg).Error
}

func (h *History) GetRecent(limit int) []messenger.Message {
	return h.getMessages(limit, nil, nil)
}

func (h *History) GetBefore(limit int, before time.Time) []messenger.Message {
	return h.getMessages(limit, &before, nil)
}

func (h *History) GetRange(limit int, since, before time.Time) []messenger.Message {
	return h.getMessages(limit, &before, &since)
}

func (h *History) getMessages(limit int, before *time.Time, since *time.Time) []messenger.Message {
	var dbMessages []db.Message
	query := db.DB.Where("contact_id = ?", h.contactID)
	if before != nil {
		query = query.Where("timestamp <= ?", before.UnixMilli())
	}
	if since != nil {
		query = query.Where("timestamp >= ?", since.UnixMilli())
	}
	query = query.Order("timestamp DESC")

	if limit > 0 {
		query = query.Limit(limit)
	}

	if err := query.Find(&dbMessages).Error; err != nil {
		return nil
	}

	// Reverse to get chronological order (oldest first)
	for i, j := 0, len(dbMessages)-1; i < j; i, j = i+1, j-1 {
		dbMessages[i], dbMessages[j] = dbMessages[j], dbMessages[i]
	}

	messages := make([]messenger.Message, len(dbMessages))
	for i, m := range dbMessages {
		messages[i] = messenger.Message{
			ID:        m.ID,
			ContactID: m.ContactID,
			Content:   m.Content,
			Type:      messenger.MessageType(m.Type),
			MediaURL:  m.MediaURL,
			Timestamp: time.UnixMilli(m.Timestamp),
			IsFromMe:  m.IsFromMe,
			Reaction:  m.Reaction,
		}
	}

	return messages
}

func (h *History) GetSince(since time.Time) []messenger.Message {
	var dbMessages []db.Message
	if err := db.DB.Where("contact_id = ? AND timestamp > ?", h.contactID, since.UnixMilli()).Order("timestamp ASC").Find(&dbMessages).Error; err != nil {
		return nil
	}

	messages := make([]messenger.Message, len(dbMessages))
	for i, m := range dbMessages {
		messages[i] = messenger.Message{
			ID:        m.ID,
			ContactID: m.ContactID,
			Content:   m.Content,
			Type:      messenger.MessageType(m.Type),
			MediaURL:  m.MediaURL,
			Timestamp: time.UnixMilli(m.Timestamp),
			IsFromMe:  m.IsFromMe,
			Reaction:  m.Reaction,
		}
	}

	return messages
}

func (h *History) Sync(ctx context.Context, m messenger.Messenger, contactID string) error {
	msgs, err := m.GetConversation(ctx, contactID, 0)
	if err != nil {
		return err
	}

	for _, msg := range msgs {
		if msg.ID == "" {
			msg.ID = uuid.New().String()
		}

		var existing db.Message
		result := db.DB.Where("id = ? OR (contact_id = ? AND timestamp = ? AND is_from_me = ?)",
			msg.ID, h.contactID, msg.Timestamp.UnixMilli(), msg.IsFromMe).First(&existing)

		if result.Error != nil {
			dbMsg := db.Message{
				ID:        msg.ID,
				ContactID: h.contactID,
				Content:   msg.Content,
				Type:      string(msg.Type),
				MediaURL:  msg.MediaURL,
				Timestamp: msg.Timestamp.UnixMilli(),
				IsFromMe:  msg.IsFromMe,
				Reaction:  msg.Reaction,
			}
			db.DB.Create(&dbMsg)
		}
	}

	return nil
}

func GetAllContactIDs() ([]string, error) {
	var contactIDs []string
	if err := db.DB.Model(&db.Message{}).Distinct("contact_id").Pluck("contact_id", &contactIDs).Error; err != nil {
		return nil, err
	}
	return contactIDs, nil
}

func GetLastMessage(contactID string) (*messenger.Message, error) {
	var dbMsg db.Message
	if err := db.DB.Where("contact_id = ?", contactID).Order("timestamp DESC").First(&dbMsg).Error; err != nil {
		return nil, err
	}

	return &messenger.Message{
		ID:        dbMsg.ID,
		ContactID: dbMsg.ContactID,
		Content:   dbMsg.Content,
		Type:      messenger.MessageType(dbMsg.Type),
		MediaURL:  dbMsg.MediaURL,
		Timestamp: time.UnixMilli(dbMsg.Timestamp),
		IsFromMe:  dbMsg.IsFromMe,
		Reaction:  dbMsg.Reaction,
	}, nil
}

func GetMessageCount(contactID string) (int, error) {
	var count int64
	if err := db.DB.Model(&db.Message{}).Where("contact_id = ?", contactID).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}
