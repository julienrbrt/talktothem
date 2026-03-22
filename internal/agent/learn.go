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

	prompt := fmt.Sprintf(`Study the messages below written by the user. Write a natural feel profile — NOT a checklist or list of rules. Describe how this person comes across when they text.

Messages:
%s

Describe their texting in 5-8 sentences, the way you'd describe someone's communication style to a friend. Cover:
- What their energy is like (laid back? intense? dry? warm? sarcastic? deadpan?) and how that shows in their word choice
- How they typically structure messages — short and clipped? chatty? one-liners? do they adapt to the other person?
- Their emoji and punctuation habits — what feels natural vs forced for them
- How they handle humor, agreements, disagreements, excitement, boredom
- What they'd naturally NEVER do (e.g. they'd never say "that's great!" with three exclamation marks if they're deadpan)
- Any recurring phrases, go-to words, or distinctive patterns

Do NOT list numbered rules. Do NOT enumerate categories. Do NOT be prescriptive ("they should..."). Just paint a detailed picture of how they text so someone could naturally mimic them.`, strings.Join(mine, "\n"))

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

	prompt := "Study the messages below — all written by the same person across conversations. Write a natural feel profile of how they text.\n\n" +
		"Messages:\n" +
		strings.Join(texts, "\n") +
		"\n\nDescribe their texting in 5-8 sentences, the way you'd describe someone's communication style to a friend. Cover:\n" +
		"- What their energy is like and how that shows in their word choice\n" +
		"- How they typically structure messages — short and clipped? chatty? one-liners? do they adapt to the other person?\n" +
		"- Their emoji and punctuation habits — what feels natural vs forced for them\n" +
		"- How they handle humor, agreements, disagreements, excitement, boredom\n" +
		"- What they'd naturally NEVER do\n" +
		"- Any recurring phrases, go-to words, or distinctive patterns\n\n" +
		"Do NOT list numbered rules. Do NOT enumerate categories. Do NOT be prescriptive. Just paint a detailed picture of how they text so someone could naturally mimic them."

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
