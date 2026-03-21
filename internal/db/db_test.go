package db

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserProfileWithLearnedFields(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "talktothem-test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	err = Init(tmpDir)
	require.NoError(t, err)

	t.Run("empty profile", func(t *testing.T) {
		profile := GetUserProfile()
		assert.Empty(t, profile.Name)
		assert.Empty(t, profile.Location)
		assert.Empty(t, profile.Timezone)
		assert.Empty(t, profile.Language)
	})

	t.Run("update learned fields only when empty", func(t *testing.T) {
		err := UpdateLearnedFields("France", "Europe/Paris", "French")
		require.NoError(t, err)

		profile := GetUserProfile()
		assert.Equal(t, "France", profile.Location)
		assert.Equal(t, "Europe/Paris", profile.Timezone)
		assert.Equal(t, "French", profile.Language)

		err = UpdateLearnedFields("Germany", "Europe/Berlin", "German")
		require.NoError(t, err)

		profile = GetUserProfile()
		assert.Equal(t, "France", profile.Location)
		assert.Equal(t, "Europe/Paris", profile.Timezone)
		assert.Equal(t, "French", profile.Language)
	})

	t.Run("manual override via update profile", func(t *testing.T) {
		profile := GetUserProfile()
		profile.Location = "Spain"
		err := UpdateUserProfile(profile)
		require.NoError(t, err)

		err = UpdateLearnedFields("Italy", "", "")
		require.NoError(t, err)

		profile = GetUserProfile()
		assert.Equal(t, "Spain", profile.Location)
	})

	t.Run("update learned field that was manually set does not override", func(t *testing.T) {
		profile := GetUserProfile()
		profile.Timezone = ""
		err := UpdateUserProfile(profile)
		require.NoError(t, err)

		err = UpdateLearnedFields("", "Europe/Madrid", "")
		require.NoError(t, err)

		profile = GetUserProfile()
		assert.Equal(t, "Europe/Madrid", profile.Timezone)
	})
}

func TestPhoneRegionHint(t *testing.T) {
	tests := []struct {
		phone    string
		expected string
	}{
		{"+33612345678", "France"},
		{"+447911123456", "United Kingdom"},
		{"+4915112345678", "Germany"},
		{"+39021234567", "Italy"},
		{"+15551234567", "United States / Canada"},
		{"+5511912345678", "Brazil"},
		{"+819012345678", "Japan"},
		{"+99999999999", ""},
		{"+1", "United States / Canada"},
	}

	for _, tt := range tests {
		t.Run(tt.phone, func(t *testing.T) {
			assert.Equal(t, tt.expected, PhoneRegionHint(tt.phone))
		})
	}
}
