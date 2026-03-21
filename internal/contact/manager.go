package contact

import (
	"context"
	"log/slog"
	"strings"

	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

type Contact = db.Contact

type Manager struct{}

func NewManager(dataPath string) (*Manager, error) {
	return &Manager{}, nil
}

func (m *Manager) Add(contact Contact) error {
	if contact.Relation == "" {
		contact.Relation = m.InferRelation(contact.Name)
	}
	return db.DB.Save(&contact).Error
}

func (m *Manager) Remove(id string) error {
	return db.DB.Delete(&Contact{}, "id = ?", id).Error
}

func (m *Manager) Get(id string) (Contact, bool) {
	var contact Contact
	result := db.DB.First(&contact, "id = ?", id)
	return contact, result.Error == nil
}

func (m *Manager) List() []Contact {
	var contacts []Contact
	db.DB.Order("COALESCE((SELECT MAX(timestamp) FROM messages WHERE messages.contact_id = contacts.id), 0) DESC").Find(&contacts)
	return contacts
}

func (m *Manager) ListActiveConversations() []Contact {
	var contacts []Contact
	db.DB.Where("EXISTS (SELECT 1 FROM messages WHERE messages.contact_id = contacts.id)").
		Order("COALESCE((SELECT MAX(timestamp) FROM messages WHERE messages.contact_id = contacts.id), 0) DESC").
		Find(&contacts)
	return contacts
}

func (m *Manager) ListEnabled() []Contact {
	var contacts []Contact
	db.DB.Where("enabled = ?", true).Order("COALESCE((SELECT MAX(timestamp) FROM messages WHERE messages.contact_id = contacts.id), 0) DESC").Find(&contacts)
	return contacts
}

func (m *Manager) SetEnabled(id string, enabled bool) error {
	return db.DB.Model(&Contact{}).Where("id = ?", id).Update("enabled", enabled).Error
}

func (m *Manager) SetStyle(id, style string) error {
	return db.DB.Model(&Contact{}).Where("id = ?", id).Update("style", style).Error
}

func (m *Manager) InferRelation(name string) string {
	name = strings.ToLower(name)

	// Family
	if containsAny(name, "mom", "mother", "maman", "mummy") {
		return "Mother"
	}
	if containsAny(name, "dad", "father", "papa", "daddy") {
		return "Father"
	}
	if containsAny(name, "bro", "brother", "frere") {
		return "Brother"
	}
	if containsAny(name, "sis", "sister", "soeur") {
		return "Sister"
	}
	if containsAny(name, "wife", "hubby", "husband", "spouse", "femme", "mari") {
		return "Spouse"
	}
	if containsAny(name, "son", "daughter", "fils", "fille") {
		return "Child"
	}
	if containsAny(name, "grandma", "grandmother", "grand-mère", "mamie") {
		return "Grandmother"
	}
	if containsAny(name, "grandpa", "grandfather", "grand-père", "papy") {
		return "Grandfather"
	}

	// Professional
	if containsAny(name, "boss", "manager", "director", "ceo") {
		return "Boss"
	}
	if containsAny(name, "colleague", "work", "office") {
		return "Colleague"
	}

	return ""
}

func containsAny(s string, keywords ...string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func (m *Manager) ImportFromMessenger(ctx context.Context, msgr messenger.Messenger, messengerName string) (int, error) {
	messengerContacts, err := msgr.GetContacts(ctx)
	if err != nil {
		return 0, err
	}

	var imported int
	for _, mc := range messengerContacts {
		if mc.Phone == "" {
			continue
		}

		existing, _ := m.Get(mc.Phone)
		if existing.ID != "" {
			continue
		}

		c := Contact{
			ID:        mc.Phone,
			Name:      mc.Name,
			Phone:     mc.Phone,
			Messenger: messengerName,
			Enabled:   false,
		}

		if err := m.Add(c); err != nil {
			slog.Warn("Failed to import contact", "phone", mc.Phone, "error", err)
			continue
		}

		imported++
	}

	return imported, nil
}
