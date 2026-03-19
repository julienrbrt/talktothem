package contact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Contact struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
}

type Manager struct {
	mu       sync.RWMutex
	contacts map[string]Contact
	filePath string
}

func NewManager(dataPath string) (*Manager, error) {
	if err := os.MkdirAll(dataPath, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	filePath := filepath.Join(dataPath, "contacts.json")
	m := &Manager{
		contacts: make(map[string]Contact),
		filePath: filePath,
	}

	if err := m.load(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.filePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read contacts file: %w", err)
	}

	var contacts []Contact
	if err := json.Unmarshal(data, &contacts); err != nil {
		return fmt.Errorf("failed to unmarshal contacts: %w", err)
	}

	for _, c := range contacts {
		m.contacts[c.ID] = c
	}

	return nil
}

func (m *Manager) save() error {
	contacts := make([]Contact, 0, len(m.contacts))
	for _, c := range m.contacts {
		contacts = append(contacts, c)
	}

	data, err := json.MarshalIndent(contacts, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal contacts: %w", err)
	}

	return os.WriteFile(m.filePath, data, 0o600)
}

func (m *Manager) Add(contact Contact) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.contacts[contact.ID] = contact
	return m.save()
}

func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.contacts, id)
	return m.save()
}

func (m *Manager) Get(id string) (Contact, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	c, ok := m.contacts[id]
	return c, ok
}

func (m *Manager) List() []Contact {
	m.mu.RLock()
	defer m.mu.RUnlock()

	contacts := make([]Contact, 0, len(m.contacts))
	for _, c := range m.contacts {
		contacts = append(contacts, c)
	}
	return contacts
}

func (m *Manager) ListEnabled() []Contact {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var contacts []Contact
	for _, c := range m.contacts {
		if c.Enabled {
			contacts = append(contacts, c)
		}
	}
	return contacts
}

func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.contacts[id]
	if !ok {
		return fmt.Errorf("contact not found: %s", id)
	}

	c.Enabled = enabled
	m.contacts[id] = c
	return m.save()
}
