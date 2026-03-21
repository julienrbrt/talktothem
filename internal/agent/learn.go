package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

type BrowserHints struct {
	Language  string
	Timezone  string
	Languages []string
}

func ExtractBrowserHints(r *http.Request) BrowserHints {
	hints := BrowserHints{}

	if tz := r.Header.Get("X-Timezone"); tz != "" {
		hints.Timezone = tz
	}
	if langHeader := r.Header.Get("Accept-Language"); langHeader != "" {
		hints.Languages = parseAcceptLanguage(langHeader)
		if len(hints.Languages) > 0 {
			hints.Language = languageToName(hints.Languages[0])
		}
	}

	return hints
}

func parseAcceptLanguage(header string) []string {
	parts := strings.Split(header, ",")
	var langs []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Split(p, ";")[0]
		p = strings.TrimSpace(p)
		if p != "" {
			langs = append(langs, strings.ToLower(p))
		}
	}
	return langs
}

var languageNames = map[string]string{
	"en":    "English",
	"en-us": "English (US)",
	"en-gb": "English (UK)",
	"fr":    "French",
	"fr-fr": "French",
	"de":    "German",
	"de-de": "German",
	"es":    "Spanish",
	"it":    "Italian",
	"pt":    "Portuguese",
	"pt-br": "Portuguese (Brazil)",
	"nl":    "Dutch",
	"ja":    "Japanese",
	"zh":    "Chinese",
	"zh-cn": "Chinese (Simplified)",
	"ko":    "Korean",
	"ru":    "Russian",
	"ar":    "Arabic",
	"hi":    "Hindi",
	"sv":    "Swedish",
	"no":    "Norwegian",
	"da":    "Danish",
	"fi":    "Finnish",
	"pl":    "Polish",
	"tr":    "Turkish",
}

func languageToName(code string) string {
	if name, ok := languageNames[strings.ToLower(code)]; ok {
		return name
	}
	return code
}

func (a *Agent) LearnFromBrowser(hints BrowserHints) {
	if hints.Language != "" || hints.Timezone != "" {
		err := db.UpdateLearnedFields("", hints.Timezone, hints.Language)
		if err != nil {
			slog.Error("Failed to save browser-learned fields", "error", err)
			return
		}
		if hints.Language != "" {
			slog.Info("Learned language from browser", "language", hints.Language)
		}
		if hints.Timezone != "" {
			slog.Info("Learned timezone from browser", "timezone", hints.Timezone)
		}
	}
}

func (a *Agent) LearnFromMessengers(ctx context.Context) {
	for name, msgr := range a.messengers {
		if msgr == nil || !msgr.IsConnected() {
			continue
		}

		linked, number, err := msgr.IsLinked(ctx)
		if err != nil || !linked {
			continue
		}

		location := db.PhoneRegionHint(number)
		if location != "" {
			err := db.UpdateLearnedFields(location, "", "")
			if err != nil {
				slog.Error("Failed to save location from messenger", "messenger", name, "error", err)
				continue
			}
			slog.Info("Learned location from messenger phone", "messenger", name, "location", location)
		}

		go func(m messenger.Messenger, messengerName string) {
			if err := db.PrefillProfileFromMessenger(ctx, m, messengerName); err != nil {
				slog.Warn("Failed to pre-fill profile from messenger", "messenger", messengerName, "error", err)
			}
		}(msgr, name)
	}
}

func (a *Agent) LearnStyle(ctx context.Context, contactID string) (string, error) {
	h, err := a.history(contactID)
	if err != nil {
		return "", err
	}

	messages := h.GetSince(time.Now().AddDate(0, -1, 0))
	if len(messages) == 0 {
		messages = h.GetRecent(100)
	}
	if len(messages) == 0 {
		return "", ErrNoMessages
	}

	var mine []string
	for _, m := range messages {
		if m.IsFromMe && m.Type == messenger.TypeText && m.Content != "" {
			mine = append(mine, m.Content)
		}
	}

	if len(mine) == 0 {
		return "", ErrNoUserMessages
	}

	prompt := fmt.Sprintf(`Analyze these messages written by a user and describe their communication style:
 %s

Describe the style in 2-3 sentences focusing on: tone, formality, emoji usage, message length, and any unique patterns.`, strings.Join(mine, "\n"))

	return a.llm.Generate(ctx, prompt)
}

func (a *Agent) LearnGlobalStyle(ctx context.Context) error {
	if a.llm == nil {
		return nil
	}

	profile := db.GetUserProfile()
	if profile.WritingStyle != "" {
		return nil
	}

	var allMessages []messenger.Message
	for _, msgr := range a.messengers {
		if msgr == nil || !msgr.IsConnected() {
			continue
		}

		contacts, err := msgr.GetContacts(ctx)
		if err != nil {
			continue
		}

		for _, c := range contacts {
			if len(allMessages) > 200 {
				break
			}

			msgs, err := msgr.GetConversation(ctx, c.ID, 20)
			if err != nil {
				continue
			}

			for _, m := range msgs {
				if m.IsFromMe && m.Type == messenger.TypeText && m.Content != "" {
					allMessages = append(allMessages, m)
				}
			}
		}

		if len(allMessages) > 200 {
			break
		}
	}

	if len(allMessages) < 10 {
		slog.Info("Not enough messages to learn global style", "count", len(allMessages))
		return nil
	}

	sort.Slice(allMessages, func(i, j int) bool {
		return allMessages[i].Timestamp.After(allMessages[j].Timestamp)
	})

	if len(allMessages) > 100 {
		allMessages = allMessages[:100]
	}

	var texts []string
	for _, m := range allMessages {
		texts = append(texts, m.Content)
	}

	prompt := "Analyze these messages from a single user across multiple conversations:\n" +
		strings.Join(texts, "\n") +
		"\n\nDescribe their overall writing style in 2-3 sentences. Focus on: tone, formality, emoji usage, " +
		"typical message length, slang, and any distinctive patterns. Be specific."

	style, err := a.llm.Generate(ctx, prompt)
	if err != nil {
		return err
	}

	profile.WritingStyle = style
	if err := db.UpdateUserProfile(profile); err != nil {
		return err
	}

	slog.Info("Learned global writing style from messages", "messageCount", len(texts))
	return nil
}
