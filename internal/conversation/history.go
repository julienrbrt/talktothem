package conversation

import (
	"context"
	"slices"
	"strings"
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
	return db.DB.Create(toDBMsg(msg, h.contactID)).Error
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

func (h *History) GetSince(since time.Time) []messenger.Message {
	return h.getMessages(0, nil, &since)
}

func (h *History) getMessages(limit int, before, since *time.Time) []messenger.Message {
	query := db.DB.Where("contact_id = ?", h.contactID)
	if before != nil {
		query = query.Where("timestamp <= ?", before.UnixMilli())
	}
	if since != nil {
		query = query.Where("timestamp >= ?", since.UnixMilli())
	}

	if limit > 0 {
		query = query.Order("timestamp DESC").Limit(limit)
	} else {
		query = query.Order("timestamp ASC")
	}

	var dbMessages []db.Message
	if err := query.Find(&dbMessages).Error; err != nil {
		return nil
	}

	if limit > 0 {
		slices.Reverse(dbMessages)
	}

	return mapMessages(dbMessages)
}

func mapMessages(dbMessages []db.Message) []messenger.Message {
	messages := make([]messenger.Message, len(dbMessages))
	for i, m := range dbMessages {
		messages[i] = toMessengerMsg(m)
	}
	return messages
}

func toMessengerMsg(m db.Message) messenger.Message {
	var mediaURLs []string
	if m.MediaURLs != "" {
		mediaURLs = strings.Split(m.MediaURLs, ",")
	}
	return messenger.Message{
		ID:        m.ID,
		ContactID: m.ContactID,
		Content:   m.Content,
		Type:      messenger.MessageType(m.Type),
		MediaURLs: mediaURLs,
		Timestamp: time.UnixMilli(m.Timestamp),
		IsFromMe:  m.IsFromMe,
		Reaction:  m.Reaction,
		IsGroup:   m.IsGroup,
	}
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
			db.DB.Create(toDBMsg(msg, h.contactID))
		}
	}

	return nil
}

func toDBMsg(msg messenger.Message, contactID string) *db.Message {
	return &db.Message{
		ID:        msg.ID,
		ContactID: contactID,
		Content:   msg.Content,
		Type:      string(msg.Type),
		MediaURLs: strings.Join(msg.MediaURLs, ","),
		Timestamp: msg.Timestamp.UnixMilli(),
		IsFromMe:  msg.IsFromMe,
		Reaction:  msg.Reaction,
		IsGroup:   msg.IsGroup,
	}
}

func (h *History) GetMessageCount() (int, error) {
	var count int64
	if err := db.DB.Model(&db.Message{}).Where("contact_id = ?", h.contactID).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

func (h *History) Clear() error {
	return db.DB.Where("contact_id = ?", h.contactID).Delete(&db.Message{}).Error
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
	msg := toMessengerMsg(dbMsg)
	return &msg, nil
}

func GetMessageCount(contactID string) (int, error) {
	var count int64
	if err := db.DB.Model(&db.Message{}).Where("contact_id = ?", contactID).Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}
