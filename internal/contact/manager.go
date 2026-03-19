package contact

import (
	"github.com/julienrbrt/talktothem/internal/db"
)

type Contact = db.Contact

type Manager struct{}

func NewManager(dataPath string) (*Manager, error) {
	return &Manager{}, nil
}

func (m *Manager) Add(contact Contact) error {
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
