package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/julienrbrt/talktothem/internal/conversation"
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

	messages := h.GetSince(time.Now().AddDate(0, -3, 0))
	if len(messages) == 0 {
		messages = h.GetRecent(500)
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

	prompt := fmt.Sprintf(`You are a behavioral analyst. Study ONLY the messages below written by ONE person (the "user") to this specific contact. Your job is to build a precise personality profile for imitation.

Messages from the user:
%s

Write a detailed style profile covering these dimensions. Be extremely specific with examples from the messages:

1. TONE & FORMALITY: What is their default emotional register? (warm, dry, sarcastic, enthusiastic, reserved, teasing, etc.) How formal or casual are they? Give examples.

2. MESSAGE LENGTH: What is their typical message length? Do they send single words, short bursts, or long paragraphs? Do they match the other person's length?

3. EMOJI HABITS: How often do they use emojis? Which ones do they repeat? Do they use them at the end of messages, in the middle, or as standalone reactions? Are they minimalist or generous?

4. PUNCTUATION & GRAMMAR: Do they use proper punctuation, no punctuation, lots of exclamation marks, ellipsis, all caps? Do they abbreviate words (thx, lol, nvm, rn)? Do they use informal spelling?

5. GREETINGS & SIGNATURES: How do they typically start and end conversations? Do they have signature phrases or habitual openers?

6. HUMOR STYLE: What kind of humor do they use? (dry wit, self-deprecating, teasing, dad jokes, none)

7. RESPONSIVENESS: Are they typically responsive with short quick replies or longer thoughtful ones?

8. LANGUAGE: What language do they write in? Do they mix languages?

Write this as a concise but rich paragraph (4-6 sentences) that would let someone perfectly mimic how this person texts. Do NOT be vague — use specific observable patterns.`, strings.Join(mine, "\n"))

	return a.llm.Generate(ctx, prompt)
}

func (a *Agent) LearnGlobalStyle(ctx context.Context) error {
	if a.llm == nil {
		return fmt.Errorf("LLM client not configured")
	}

	slog.Info("Learning global style: fetching messages from local history")

	allMessages := conversation.GetOutgoingMessages(500)

	if len(allMessages) == 0 {
		return fmt.Errorf("no outgoing messages found in history")
	}

	sort.Slice(allMessages, func(i, j int) bool {
		return allMessages[i].Timestamp.After(allMessages[j].Timestamp)
	})

	if len(allMessages) > 500 {
		allMessages = allMessages[:500]
	}

	var texts []string
	for _, m := range allMessages {
		texts = append(texts, m.Content)
	}

	prompt := "You are a behavioral analyst. Study ONLY the messages below — ALL written by the SAME person across multiple conversations. Build a comprehensive personality profile for imitation.\n\n" +
		"Messages from the user:\n" +
		strings.Join(texts, "\n") +
		"\n\nWrite a detailed style profile covering these dimensions. Be extremely specific with examples:\n\n" +
		"1. TONE & FORMALITY: Default emotional register? (warm, dry, sarcastic, enthusiastic, reserved, teasing) Formal or casual?\n" +
		"2. MESSAGE LENGTH: Single words, short bursts, or long paragraphs? Do they adapt length to the conversation?\n" +
		"3. EMOJI HABITS: Frequency? Which ones repeat? Placement in messages? Minimalist or generous?\n" +
		"4. PUNCTUATION & GRAMMAR: Proper punctuation or none? Lots of !, ..., ALL CAPS? Abbreviations (thx, lol, nvm)? Informal spelling?\n" +
		"5. GREETINGS & SIGNATURES: How do they start/end conversations? Signature phrases or habitual openers?\n" +
		"6. HUMOR STYLE: Dry wit, self-deprecating, teasing, dad jokes, dark humor, or none?\n" +
		"7. RESPONSIVENESS: Quick short replies or longer thoughtful ones?\n" +
		"8. LANGUAGE: Primary language? Do they mix languages or code-switch?\n" +
		"9. UNIQUE QUIRKS: Any distinctive patterns, recurring phrases, typing habits?\n\n" +
		"Write a concise but rich paragraph (4-6 sentences) that would let someone perfectly mimic this person. Do NOT be vague — use specific observable patterns."

	style, err := a.llm.Generate(ctx, prompt)
	if err != nil {
		return err
	}

	profile := db.GetUserProfile()
	profile.WritingStyle = style
	if err := db.UpdateUserProfile(profile); err != nil {
		return err
	}

	slog.Info("Learned global writing style from messages", "messageCount", len(texts))
	return nil
}
