package contact

import (
	"os"
	"testing"

	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = db.Init(tmpDir)
	require.NoError(t, err)

	m, err := NewManager(tmpDir)
	require.NoError(t, err)

	t.Run("add contact", func(t *testing.T) {
		c := Contact{
			ID:          "+1234567890",
			Name:        "Test Contact",
			Phone:       "+1234567890",
			Enabled:     false,
			Description: "A test contact",
		}
		err := m.Add(c)
		require.NoError(t, err)

		got, ok := m.Get("+1234567890")
		assert.True(t, ok)
		assert.Equal(t, "Test Contact", got.Name)
	})

	t.Run("list contacts", func(t *testing.T) {
		contacts := m.List()
		assert.Len(t, contacts, 1)
	})

	t.Run("set enabled", func(t *testing.T) {
		err := m.SetEnabled("+1234567890", true)
		require.NoError(t, err)

		got, ok := m.Get("+1234567890")
		assert.True(t, ok)
		assert.True(t, got.Enabled)

		enabled := m.ListEnabled()
		assert.Len(t, enabled, 1)
	})

	t.Run("remove contact", func(t *testing.T) {
		err := m.Remove("+1234567890")
		require.NoError(t, err)

		_, ok := m.Get("+1234567890")
		assert.False(t, ok)
	})

	t.Run("infer relation", func(t *testing.T) {
		cases := []struct {
			name     string
			expected string
		}{
			{"Mom", "Mother"},
			{"My Mother", "Mother"},
			{"Dad", "Father"},
			{"Brother John", "Brother"},
			{"Sister Mary", "Sister"},
			{"Wife", "Spouse"},
			{"The Boss", "Boss"},
			{"Work Colleague", "Colleague"},
			{"Random Friend", ""},
		}

		for _, tc := range cases {
			assert.Equal(t, tc.expected, m.InferRelation(tc.name), tc.name)
		}
	})

	t.Run("add contact with inferred relation", func(t *testing.T) {
		c := Contact{
			ID:    "+999",
			Name:  "Mom",
			Phone: "+999",
		}
		err := m.Add(c)
		require.NoError(t, err)

		got, ok := m.Get("+999")
		assert.True(t, ok)
		assert.Equal(t, "Mother", got.Relation)
	})
}

func TestManagerPersistence(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = db.Init(tmpDir)
	require.NoError(t, err)

	m1, err := NewManager(tmpDir)
	require.NoError(t, err)

	c := Contact{
		ID:      "+1234567890",
		Name:    "Test",
		Phone:   "+1234567890",
		Enabled: true,
	}
	err = m1.Add(c)
	require.NoError(t, err)

	m2, err := NewManager(tmpDir)
	require.NoError(t, err)

	got, ok := m2.Get("+1234567890")
	assert.True(t, ok)
	assert.Equal(t, "Test", got.Name)
	assert.True(t, got.Enabled)
}

func TestManagerEmptyDirectory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = db.Init(tmpDir)
	require.NoError(t, err)

	m, err := NewManager(tmpDir)
	require.NoError(t, err)

	contacts := m.List()
	assert.Empty(t, contacts)
}
