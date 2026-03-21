package agent

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseAcceptLanguage(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected []string
	}{
		{
			name:     "single language",
			header:   "en-US",
			expected: []string{"en-us"},
		},
		{
			name:     "multiple languages with quality",
			header:   "en-US,en;q=0.9,fr-FR;q=0.8",
			expected: []string{"en-us", "en", "fr-fr"},
		},
		{
			name:     "multiple with spaces",
			header:   "fr-FR, fr;q=0.9, en-US;q=0.8, en;q=0.7",
			expected: []string{"fr-fr", "fr", "en-us", "en"},
		},
		{
			name:     "empty header",
			header:   "",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseAcceptLanguage(tt.header)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLanguageToName(t *testing.T) {
	tests := []struct {
		code     string
		expected string
	}{
		{"en", "English"},
		{"en-us", "English (US)"},
		{"en-gb", "English (UK)"},
		{"fr", "French"},
		{"fr-fr", "French"},
		{"de", "German"},
		{"es", "Spanish"},
		{"pt-br", "Portuguese (Brazil)"},
		{"unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			assert.Equal(t, tt.expected, languageToName(tt.code))
		})
	}
}

func TestExtractBrowserHints(t *testing.T) {
	t.Run("with timezone header", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Timezone", "Europe/Paris")

		hints := ExtractBrowserHints(req)
		assert.Equal(t, "Europe/Paris", hints.Timezone)
	})

	t.Run("with accept language", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Language", "fr-FR,fr;q=0.9,en;q=0.8")

		hints := ExtractBrowserHints(req)
		assert.Equal(t, "French", hints.Language)
		assert.Equal(t, []string{"fr-fr", "fr", "en"}, hints.Languages)
	})

	t.Run("with both headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("X-Timezone", "America/New_York")

		hints := ExtractBrowserHints(req)
		assert.Equal(t, "English (US)", hints.Language)
		assert.Equal(t, "America/New_York", hints.Timezone)
	})

	t.Run("no headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)

		hints := ExtractBrowserHints(req)
		assert.Equal(t, "", hints.Language)
		assert.Equal(t, "", hints.Timezone)
	})
}
