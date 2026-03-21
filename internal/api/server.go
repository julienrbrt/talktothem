//go:generate go run generate.go

package api

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/julienrbrt/talktothem/internal/agent"
	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/conversation"
	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/llm"
	"github.com/julienrbrt/talktothem/internal/messenger"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type Server struct {
	ctx        context.Context
	server     *http.Server
	router     *chi.Mux
	agent      *agent.Agent
	contacts   *contact.Manager
	messengers map[string]messenger.Messenger
	config     *db.Config
	hub        *Hub
	templates  *template.Template
	assets     fs.FS
}

type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan []byte
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case conn := <-h.register:
			h.clients[conn] = true
		case conn := <-h.unregister:
			delete(h.clients, conn)
			conn.Close()
		case message := <-h.broadcast:
			for conn := range h.clients {
				if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
					delete(h.clients, conn)
					conn.Close()
				}
			}
		}
	}
}

type MessageEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

//go:embed templates/*.html templates/partials/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

func NewServer(ctx context.Context, addr string, ag *agent.Agent, cm *contact.Manager, msgrs map[string]messenger.Messenger, cfg *db.Config) *Server {
	r := chi.NewRouter()

	tmpl := template.Must(template.ParseFS(templatesFS,
		"templates/base.html",
		"templates/index.html",
		"templates/settings.html",
		"templates/profile.html",
		"templates/partials/*.html",
	))

	s := &Server{
		ctx:        ctx,
		router:     r,
		agent:      ag,
		contacts:   cm,
		messengers: msgrs,
		config:     cfg,
		hub:        NewHub(),
		templates:  tmpl,
		assets:     staticFS,
	}

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	r.Use(s.learnFromBrowserMiddleware)

	r.Route("/api", func(r chi.Router) {
		// Onboarding
		r.Get("/status", s.getStatus)
		r.Post("/onboarding", s.completeOnboarding)

		// Messenger device linking
		r.Get("/messenger/{type}/link/start", s.startMessengerLinking)
		r.Get("/messenger/{type}/link/status", s.getMessengerLinkStatus)
		r.Post("/messenger/{type}/unlink", s.unlinkMessenger)

		// Configuration
		r.Get("/config", s.getConfig)
		r.Put("/config", s.updateConfig)

		// Contacts
		r.Get("/contacts", s.listContacts)
		r.Get("/contacts/all", s.listAllContacts)
		r.Post("/contacts", s.createContact)
		r.Post("/messenger/{type}/contacts/import", s.importContactsFromMessenger)
		r.Get("/contacts/{id}", s.getContact)
		r.Put("/contacts/{id}", s.updateContact)
		r.Delete("/contacts/{id}", s.deleteContact)
		r.Post("/contacts/{id}/enable", s.enableContact)
		r.Post("/contacts/{id}/disable", s.disableContact)
		r.Get("/contacts/{id}/conversation", s.getConversation)
		r.Post("/contacts/{id}/message", s.sendMessage)
		r.Post("/contacts/{id}/sync", s.syncConversation)
		r.Delete("/contacts/{id}/history", s.clearHistory)
		r.Post("/contacts/{id}/learn-style", s.learnStyle)
		r.Post("/contacts/{id}/initiate", s.initiateConversation)
		r.Get("/contacts/{id}/response-check", s.checkResponse)
		r.Get("/contacts/{id}/summary", s.getSummary)

		// Media
		r.Get("/media", s.getMedia)

		// User Profile
		r.Get("/profile", s.getUserProfile)
		r.Put("/profile", s.updateUserProfile)
		r.Post("/profile/learn-style", s.learnGlobalStyle)
	})

	r.Get("/ws", s.handleWebSocket)

	r.Get("/", s.indexPage)
	r.Get("/settings", s.settingsPage)
	r.Get("/profile", s.profilePage)
	r.Get("/conversations/{id}", s.conversationDetailPage)

	if s.assets != nil {
		FileServer(r, "/static", s.assets)
	}

	s.server = &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return s
}

func FileServer(r chi.Router, path string, root fs.FS) {
	fs := http.FileServer(http.FS(root))
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		fs.ServeHTTP(w, r)
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) learnFromBrowserMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.agent != nil {
			hints := agent.ExtractBrowserHints(r)
			if hints.Language != "" || hints.Timezone != "" {
				s.agent.LearnFromBrowser(hints)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Run() error {
	go s.hub.Run()
	if s.agent != nil {
		go s.listenForAgentResponses()
		go s.listenForAgentQueued()
	}
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) getMessenger(contactID string) messenger.Messenger {
	c, ok := s.contacts.Get(contactID)
	if !ok {
		return nil
	}
	msgr, ok := s.messengers[c.Messenger]
	if !ok || msgr == nil {
		return nil
	}
	return msgr
}

func (s *Server) sendTyping(contactID string, show bool) {
	cfg := db.GetConfig()
	if cfg == nil || cfg.DisableDelay {
		return
	}
	if msgr := s.getMessenger(contactID); msgr != nil {
		if err := msgr.SendTypingIndicator(context.Background(), contactID, show); err != nil {
			slog.Error("Error with typing indicator", "show", show, "error", err)
		}
	}
}

func (s *Server) listenForAgentResponses() {
	for resp := range s.agent.Outbox() {
		if resp.TypingDelay > 0 {
			time.Sleep(resp.TypingDelay)
		}
		s.sendTyping(resp.ContactID, false)

		if msgr := s.getMessenger(resp.ContactID); msgr != nil {
			if err := msgr.SendMessage(context.Background(), resp.ContactID, resp.Content); err != nil {
				slog.Error("Error sending agent message to messenger", "error", err)
			}
		}

		_ = s.agent.RecordMessage(context.Background(), messenger.Message{
			ContactID: resp.ContactID,
			Content:   resp.Content,
			Type:      messenger.TypeText,
			Timestamp: time.Now(),
			IsFromMe:  true,
		})

		s.broadcastEvent("agent_response", map[string]string{
			"contactId": resp.ContactID,
			"content":   resp.Content,
		})
	}
}

func (s *Server) listenForAgentQueued() {
	for q := range s.agent.Queued() {
		s.sendTyping(q.ContactID, true)

		s.broadcastEvent("queued_response", map[string]any{
			"contactId": q.ContactID,
			"content":   q.Content,
			"sendAt":    q.SendAt,
		})
	}
}

func (s *Server) getSummary(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	beforeStr := r.URL.Query().Get("before")

	before := time.Now()
	if beforeStr != "" {
		if t, err := time.Parse(time.RFC3339, beforeStr); err == nil {
			before = t
		} else if ms, err := time.Parse("2006-01-02 15:04:05", beforeStr); err == nil {
			before = ms
		}
	}

	summary, err := s.agent.Summarize(r.Context(), id, before)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"summary": summary})
}

func (s *Server) getMedia(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	// Simple security check: path should not contain ..
	if strings.Contains(path, "..") {
		http.Error(w, "invalid path", http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, path)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.hub.register <- conn

	defer func() {
		s.hub.unregister <- conn
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("Error encoding JSON response", "error", err)
	}
}

func (s *Server) broadcastEvent(eventType string, payload any) {
	event := MessageEvent{Type: eventType, Payload: payload}
	data, _ := json.Marshal(event)
	s.hub.broadcast <- data
}

func (s *Server) BroadcastMessage(msg messenger.Message) {
	s.broadcastEvent("new_message", messageToResponse(msg))
}

type ContactResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Phone        string `json:"phone"`
	Messenger    string `json:"messenger"`
	Enabled      bool   `json:"enabled"`
	Description  string `json:"description"`
	Style        string `json:"style"`
	Relation     string `json:"relation"`
	BannedTopics string `json:"bannedTopics"`
}

func contactToResponse(c contact.Contact) ContactResponse {
	name := c.Name
	if name == "" {
		name = c.Phone
	}
	return ContactResponse{
		ID:           c.ID,
		Name:         name,
		Phone:        c.Phone,
		Messenger:    c.Messenger,
		Enabled:      c.Enabled,
		Description:  c.Description,
		Style:        c.Style,
		Relation:     c.Relation,
		BannedTopics: c.BannedTopics,
	}
}

type MessageResponse struct {
	ID        string    `json:"id"`
	ContactID string    `json:"contactId"`
	Content   string    `json:"content"`
	Type      string    `json:"type"`
	MediaURLs []string  `json:"mediaUrls,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	IsFromMe  bool      `json:"isFromMe"`
	Reaction  string    `json:"reaction,omitempty"`
}

func messageToResponse(m messenger.Message) MessageResponse {
	return MessageResponse{
		ID:        m.ID,
		ContactID: m.ContactID,
		Content:   m.Content,
		Type:      string(m.Type),
		MediaURLs: m.MediaURLs,
		Timestamp: m.Timestamp,
		IsFromMe:  m.IsFromMe,
		Reaction:  m.Reaction,
	}
}

type SidebarData struct {
	Active   []ContactResponse
	Inactive []ContactResponse
}

func (s *Server) getSidebarData() SidebarData {
	contacts := s.contacts.List()

	var active, inactive []ContactResponse
	for _, c := range contacts {
		resp := contactToResponse(c)
		if c.Enabled {
			active = append(active, resp)
		} else {
			inactive = append(inactive, resp)
		}
	}

	return SidebarData{
		Active:   active,
		Inactive: inactive,
	}
}

func (s *Server) getMessengerStatus() (bool, bool) {
	hasMessengerConnected := false
	hasMessengerEnabled := false

	for _, name := range messenger.Supported {
		msgr := s.messengers[name]
		messengerCfg := db.GetMessengerConfig(name)
		if messengerCfg != nil && messengerCfg.Enabled {
			hasMessengerEnabled = true
			if msgr != nil && msgr.IsConnected() {
				hasMessengerConnected = true
			}
		}
	}
	return hasMessengerConnected, hasMessengerEnabled
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request) {
	data := s.getSidebarData()
	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "contacts", data); err != nil {
		slog.Error("Error executing contacts template", "error", err)
	}
}

func (s *Server) listAllContacts(w http.ResponseWriter, r *http.Request) {
	contacts := s.contacts.List()

	var response []ContactResponse
	for _, c := range contacts {
		response = append(response, contactToResponse(c))
	}

	if response == nil {
		response = []ContactResponse{}
	}

	writeJSON(w, response)
}

type CreateContactRequest struct {
	Name         string
	Phone        string
	Description  string
	Relation     string
	BannedTopics string
}

func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req := CreateContactRequest{
		Name:         r.FormValue("name"),
		Phone:        r.FormValue("phone"),
		Description:  r.FormValue("description"),
		Relation:     r.FormValue("relation"),
		BannedTopics: r.FormValue("bannedTopics"),
	}

	c := contact.Contact{
		ID:           req.Phone,
		Name:         req.Name,
		Phone:        req.Phone,
		Description:  req.Description,
		Relation:     req.Relation,
		BannedTopics: req.BannedTopics,
		Enabled:      true,
	}

	if err := s.contacts.Add(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return updated contact list for Go Templates
	contacts := s.contacts.ListActiveConversations()
	var response []ContactResponse
	for _, ct := range contacts {
		response = append(response, contactToResponse(ct))
	}
	if response == nil {
		response = []ContactResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "contacts", response); err != nil {
		slog.Error("Error executing contacts template", "error", err)
	}
}

func (s *Server) getContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	c, ok := s.contacts.Get(id)
	if !ok {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}

	writeJSON(w, contactToResponse(c))
}

type UpdateContactRequest struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Style        string `json:"style"`
	Relation     string `json:"relation"`
	BannedTopics string `json:"bannedTopics"`
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	c, ok := s.contacts.Get(id)
	if !ok {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}

	var req UpdateContactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c.Name = req.Name
	c.Description = req.Description
	c.Style = req.Style
	c.Relation = req.Relation
	c.BannedTopics = req.BannedTopics

	if err := s.contacts.Add(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcastEvent("contact_updated", contactToResponse(c))
	writeJSON(w, contactToResponse(c))
}

func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := s.contacts.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcastEvent("contact_deleted", map[string]string{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) enableContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := s.contacts.SetEnabled(id, true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Always learn the conversation style first when enabling
	style, _ := s.agent.LearnStyle(r.Context(), id)
	if err := s.contacts.SetStyle(id, style); err != nil {
		slog.Error("Error setting contact style", "error", err)
	}
	c, _ := s.contacts.Get(id)

	// Check for unanswered messages
	check, _ := s.agent.CheckResponse(id, 48*time.Hour)

	response := map[string]any{
		"contact":         contactToResponse(c),
		"hasUnanswered":   check.Needed,
		"lastSender":      check.LastSender,
		"lastMessageTime": check.LastAt,
	}

	s.broadcastEvent("contact_enabled", contactToResponse(c))
	writeJSON(w, response)
}

func (s *Server) disableContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := s.contacts.SetEnabled(id, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Stop any pending response in the agent
	if s.agent != nil {
		s.agent.Stop(id)
	}

	c, _ := s.contacts.Get(id)
	s.broadcastEvent("contact_disabled", contactToResponse(c))
	writeJSON(w, contactToResponse(c))
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	h, err := conversation.NewHistory("", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	messages := h.GetRecent(0)

	var response []MessageResponse
	for _, m := range messages {
		response = append(response, messageToResponse(m))
	}

	if response == nil {
		response = []MessageResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "messages", response); err != nil {
		slog.Error("Error executing messages template", "error", err)
	}
}

type SendMessageRequest struct {
	Content string `json:"content"`
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	c, ok := s.contacts.Get(id)
	if !ok {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}

	msgr, ok := s.messengers[c.Messenger]
	if !ok || msgr == nil {
		http.Error(w, "messenger not connected", http.StatusServiceUnavailable)
		return
	}

	if err := msgr.SendMessage(r.Context(), id, req.Content); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	msg := messenger.Message{
		ContactID: id,
		Content:   req.Content,
		Type:      messenger.TypeText,
		Timestamp: time.Now(),
		IsFromMe:  true,
	}

	if err := s.agent.RecordMessage(r.Context(), msg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcastEvent("message_sent", messageToResponse(msg))
	writeJSON(w, messageToResponse(msg))
}

func (s *Server) syncConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	c, ok := s.contacts.Get(id)
	if !ok {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}

	msgr, ok := s.messengers[c.Messenger]
	if !ok || msgr == nil {
		http.Error(w, "messenger not connected", http.StatusServiceUnavailable)
		return
	}

	if err := s.agent.SyncHistory(r.Context(), msgr, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Try to learn style if not set
	if c.Style == "" {
		style, _ := s.agent.LearnStyle(r.Context(), id)
		if style != "" {
			_ = s.contacts.SetStyle(id, style)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := s.agent.ClearHistory(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) learnStyle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	style, err := s.agent.LearnStyle(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := s.contacts.SetStyle(id, style); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, _ := s.contacts.Get(id)
	s.broadcastEvent("style_learned", contactToResponse(c))
	writeJSON(w, map[string]string{"style": style})
}

func (s *Server) initiateConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	msg, err := s.agent.Initiate(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if msg != "" {
		c, ok := s.contacts.Get(id)
		if ok {
			msgr, ok := s.messengers[c.Messenger]
			if ok && msgr != nil {
				if err := msgr.SendMessage(r.Context(), id, msg); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				_ = s.agent.RecordMessage(r.Context(), messenger.Message{
					ContactID: id,
					Content:   msg,
					Type:      messenger.TypeText,
					Timestamp: time.Now(),
					IsFromMe:  true,
				})
			}
		}
	}

	writeJSON(w, map[string]string{"message": msg})
}

func (s *Server) checkResponse(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	check, err := s.agent.CheckResponse(id, 24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, check)
}

type StatusResponse struct {
	Onboarded        bool                       `json:"onboarded"`
	HasMessenger     bool                       `json:"hasMessenger"`
	HasAPIKey        bool                       `json:"hasApiKey"`
	MessengerNumber  string                     `json:"messengerNumber,omitempty"`
	ConnectedCount   int                        `json:"connectedCount"`
	ConnectionStatus string                     `json:"connectionStatus"`
	Messengers       map[string]MessengerStatus `json:"messengers,omitempty"`
}

type MessengerStatus struct {
	Available bool   `json:"available"`
	Enabled   bool   `json:"enabled"`
	Phone     string `json:"phone,omitempty"`
	Connected bool   `json:"connected,omitempty"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	hasAPIKey := s.config.APIKey != ""

	contacts := s.contacts.List()
	var connected int
	for _, c := range contacts {
		if c.Enabled {
			connected++
		}
	}

	messengers := make(map[string]MessengerStatus)
	hasMessengerConfig := false
	hasAnyMessengerLinked := false
	hasMessengerConnected := false

	for _, t := range messenger.Supported {
		cfg := db.GetMessengerConfig(t)
		msgr := s.messengers[t]

		status := MessengerStatus{Available: msgr != nil}

		if cfg != nil {
			status.Enabled = cfg.Enabled
			if cfg.Enabled {
				hasMessengerConfig = true
			}
		}

		if msgr != nil {
			linked, number, err := msgr.IsLinked(r.Context())
			if err == nil && linked {
				hasAnyMessengerLinked = true
				if number != "" {
					status.Phone = number
				}
			}
			status.Connected = msgr.IsConnected()
		}

		messengers[t] = status
		if status.Connected {
			hasMessengerConnected = true
		}
	}

	connectionStatus := "disconnected"
	if hasMessengerConnected {
		connectionStatus = "connected"
	}

	response := StatusResponse{
		Onboarded:        hasAPIKey && hasMessengerConfig,
		HasMessenger:     hasAnyMessengerLinked,
		HasAPIKey:        hasAPIKey,
		ConnectedCount:   connected,
		ConnectionStatus: connectionStatus,
		Messengers:       messengers,
	}

	writeJSON(w, response)
}

type MessengerLinkResponse struct {
	QRCode string `json:"qrCode"` // base64 encoded PNG
}

func (s *Server) startMessengerLinking(w http.ResponseWriter, r *http.Request) {
	mt := chi.URLParam(r, "type")
	msgr, ok := s.messengers[mt]
	if !ok || msgr == nil {
		http.Error(w, "messenger type not supported", http.StatusBadRequest)
		return
	}

	qrCode, err := msgr.StartLinking(r.Context(), "TalkToThem")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := MessengerLinkResponse{
		QRCode: base64.StdEncoding.EncodeToString(qrCode),
	}

	writeJSON(w, response)
}

type MessengerLinkStatusResponse struct {
	Linked bool   `json:"linked"`
	Number string `json:"number,omitempty"`
}

func (s *Server) ensureConnected(msgr messenger.Messenger) {
	if msgr != nil && !msgr.IsConnected() {
		slog.Info("Connecting to messenger", "name", msgr.Name())
		if err := msgr.Connect(s.ctx); err != nil {
			slog.Warn("Failed to connect to messenger", "name", msgr.Name(), "error", err)
			return
		}
		msgr.StartReceiving(s.ctx)
	}
}

func (s *Server) getMessengerLinkStatus(w http.ResponseWriter, r *http.Request) {
	mt := chi.URLParam(r, "type")
	msgr, ok := s.messengers[mt]
	if !ok || msgr == nil {
		http.Error(w, "messenger type not supported", http.StatusBadRequest)
		return
	}

	linked, number, err := msgr.IsLinked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if linked {
		cfg := db.GetMessengerConfig(mt)
		if cfg == nil {
			cfg = &db.MessengerConfig{
				Type: mt,
			}
		}
		if !cfg.Enabled {
			cfg.Enabled = true
			_ = db.SaveMessengerConfig(cfg)
		}
		s.ensureConnected(msgr)
	}

	response := MessengerLinkStatusResponse{
		Linked: linked,
		Number: number,
	}

	writeJSON(w, response)
}

type OnboardingRequest struct {
	APIKey  string
	Model   string
	BaseURL string
	Type    string
}

func (s *Server) completeOnboarding(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req := OnboardingRequest{
		APIKey:  r.FormValue("apiKey"),
		Model:   r.FormValue("model"),
		BaseURL: r.FormValue("baseUrl"),
		Type:    r.FormValue("type"),
	}

	if req.Type == "" {
		req.Type = "signal" // Default fallback
	}

	msgr, ok := s.messengers[req.Type]
	if !ok || msgr == nil {
		http.Error(w, "messenger type not supported", http.StatusBadRequest)
		return
	}

	// Get the number from the linked messenger device
	linked, _, err := msgr.IsLinked(r.Context())
	if err != nil {
		http.Error(w, "failed to check messenger link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !linked {
		http.Error(w, "messenger device not linked", http.StatusBadRequest)
		return
	}

	if req.APIKey == "" {
		http.Error(w, "api key is required", http.StatusBadRequest)
		return
	}

	// Save LLM config
	s.config.APIKey = req.APIKey
	if req.Model != "" {
		s.config.Model = req.Model
	}
	if req.BaseURL != "" {
		s.config.BaseURL = req.BaseURL
	}
	if err := db.UpdateConfig(s.config); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update agent LLM client
	if s.agent != nil {
		llmClient := llm.NewClient(llm.Config{
			APIKey:  s.config.APIKey,
			BaseURL: s.config.BaseURL,
			Model:   s.config.Model,
		})
		s.agent.SetLLM(llmClient)
		s.agent.SetVision(llmClient)
	}

	// Save messenger config with number from linked device
	messengerCfg := db.GetMessengerConfig(msgr.Name())
	if messengerCfg == nil {
		messengerCfg = &db.MessengerConfig{
			Type: msgr.Name(),
		}
	}
	messengerCfg.Enabled = true
	if err := db.SaveMessengerConfig(messengerCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.ensureConnected(msgr)

	go func() {
		ctx := context.Background()
		if err := db.PrefillProfileFromMessenger(ctx, msgr, req.Type); err != nil {
			slog.Warn("Failed to pre-fill profile after onboarding", "messenger", req.Type, "error", err)
		}

		imported, err := s.contacts.ImportFromMessenger(ctx, msgr, req.Type)
		if err != nil {
			slog.Warn("Failed to import contacts after onboarding", "messenger", req.Type, "error", err)
			return
		}
		if imported > 0 {
			slog.Info("Imported contacts after onboarding", "messenger", req.Type, "count", imported)
		}

		if s.agent != nil {
			synced, err := s.agent.SyncAllHistory(ctx, req.Type)
			if err != nil {
				slog.Warn("Failed to sync all history after onboarding", "messenger", req.Type, "error", err)
			} else if synced > 0 {
				slog.Info("Synced history after onboarding", "messenger", req.Type, "count", synced)
			}
		}
	}()

	writeJSON(w, map[string]bool{"success": true})
}

type MessengerContact struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

func (s *Server) importContactsFromMessenger(w http.ResponseWriter, r *http.Request) {
	mt := chi.URLParam(r, "type")
	msgr, ok := s.messengers[mt]
	if !ok || msgr == nil {
		http.Error(w, "messenger type not supported", http.StatusBadRequest)
		return
	}

	// Get number from linked device
	linked, _, err := msgr.IsLinked(r.Context())
	if err != nil {
		http.Error(w, "failed to check messenger link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !linked {
		http.Error(w, "messenger device not linked", http.StatusBadRequest)
		return
	}

	messengerContacts, err := msgr.GetContacts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, mc := range messengerContacts {
		if mc.Phone == "" {
			continue
		}

		existing, _ := s.contacts.Get(mc.Phone)
		if existing.ID != "" {
			continue
		}

		c := contact.Contact{
			ID:        mc.Phone,
			Name:      mc.Name,
			Phone:     mc.Phone,
			Messenger: mt,
			Enabled:   false,
		}

		if err := s.contacts.Add(c); err != nil {
			continue
		}
	}

	// Return updated contact list for Go Templates
	contacts := s.contacts.ListActiveConversations()
	var response []ContactResponse
	for _, ct := range contacts {
		response = append(response, contactToResponse(ct))
	}
	if response == nil {
		response = []ContactResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "contacts", response); err != nil {
		slog.Error("Error executing contacts template", "error", err)
	}
}

type DashboardData struct {
	TotalActiveAgents int
	TotalMessages     int
	RecentActivity    []ActivitySummary
}

type ActivitySummary struct {
	ContactID string
	Name      string
	Summary   string
	LastAt    time.Time
	Messenger string
}

type PageData struct {
	Onboarded     bool
	HasMessenger  bool
	SidebarData   SidebarData
	Page          string
	Contact       contact.Contact
	Messages      []MessageResponse
	DashboardData DashboardData
}

func (s *Server) indexPage(w http.ResponseWriter, r *http.Request) {
	hasAPIKey := s.config.APIKey != ""
	hasMessenger, hasMessengerConfig := s.getMessengerStatus()

	contacts := s.contacts.List()
	activeCount := 0
	for _, c := range contacts {
		if c.Enabled {
			activeCount++
		}
	}

	var totalMessages int64
	db.DB.Model(&db.Message{}).Count(&totalMessages)

	var activity []ActivitySummary
	activeConvos := s.contacts.ListActiveConversations()
	for i, c := range activeConvos {
		if i >= 5 {
			break
		}
		lastMsg, _ := conversation.GetLastMessage(c.ID)
		lastAt := time.Now()
		if lastMsg != nil {
			lastAt = lastMsg.Timestamp
		}

		activity = append(activity, ActivitySummary{
			ContactID: c.ID,
			Name:      c.Name,
			LastAt:    lastAt,
			Messenger: c.Messenger,
		})
	}

	data := PageData{
		Page:         "index",
		Onboarded:    hasAPIKey && hasMessengerConfig,
		HasMessenger: hasMessenger,
		SidebarData:  s.getSidebarData(),
		DashboardData: DashboardData{
			TotalActiveAgents: activeCount,
			TotalMessages:     int(totalMessages),
			RecentActivity:    activity,
		},
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) conversationDetailPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	c, ok := s.contacts.Get(id)
	if !ok {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}

	h, err := conversation.NewHistory("", id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	messages := h.GetRecent(20)
	var msgResponses []MessageResponse
	for _, m := range messages {
		msgResponses = append(msgResponses, messageToResponse(m))
	}
	if msgResponses == nil {
		msgResponses = []MessageResponse{}
	}

	hasAPIKey := s.config.APIKey != ""
	hasMessenger, hasMessengerConfig := s.getMessengerStatus()

	data := PageData{
		Page:         "conversation",
		Onboarded:    hasAPIKey && hasMessengerConfig,
		Contact:      c,
		Messages:     msgResponses,
		HasMessenger: hasMessenger,
		SidebarData:  s.getSidebarData(),
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type UserProfileResponse struct {
	Name          string `json:"name"`
	About         string `json:"about"`
	FamilyContext string `json:"familyContext"`
	WorkContext   string `json:"workContext"`
	WritingStyle  string `json:"writingStyle"`
	Location      string `json:"location"`
	Timezone      string `json:"timezone"`
	Language      string `json:"language"`
}

func toUserProfileResponse(p *db.UserProfile) UserProfileResponse {
	return UserProfileResponse{
		Name:          p.Name,
		About:         p.About,
		FamilyContext: p.FamilyContext,
		WorkContext:   p.WorkContext,
		WritingStyle:  p.WritingStyle,
		Location:      p.Location,
		Timezone:      p.Timezone,
		Language:      p.Language,
	}
}

func (s *Server) getUserProfile(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, toUserProfileResponse(db.GetUserProfile()))
}

func (s *Server) learnGlobalStyle(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		http.Error(w, "agent not available", http.StatusServiceUnavailable)
		return
	}

	if err := s.agent.LearnGlobalStyle(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, toUserProfileResponse(db.GetUserProfile()))
}

type UpdateUserProfileRequest struct {
	Name          string `json:"name"`
	About         string `json:"about"`
	FamilyContext string `json:"familyContext"`
	WorkContext   string `json:"workContext"`
	WritingStyle  string `json:"writingStyle"`
	Location      string `json:"location"`
	Timezone      string `json:"timezone"`
	Language      string `json:"language"`
}

func (s *Server) updateUserProfile(w http.ResponseWriter, r *http.Request) {
	var req UpdateUserProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	profile := db.GetUserProfile()
	profile.Name = req.Name
	profile.About = req.About
	profile.FamilyContext = req.FamilyContext
	profile.WorkContext = req.WorkContext
	profile.WritingStyle = req.WritingStyle
	profile.Location = req.Location
	profile.Timezone = req.Timezone
	profile.Language = req.Language

	if err := db.UpdateUserProfile(profile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, toUserProfileResponse(profile))
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "settings")
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "profile")
}

func (s *Server) renderPage(w http.ResponseWriter, page string) {
	hasAPIKey := s.config.APIKey != ""
	hasMessenger, hasMessengerConfig := s.getMessengerStatus()

	data := PageData{
		Page:         page,
		Onboarded:    hasAPIKey && hasMessengerConfig,
		HasMessenger: hasMessenger,
		SidebarData:  s.getSidebarData(),
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type ConfigResponse struct {
	APIKey       string `json:"apiKey"`
	Model        string `json:"model"`
	BaseURL      string `json:"baseUrl"`
	DisableDelay bool   `json:"disableDelay"`
}

func toConfigResponse(c *db.Config) ConfigResponse {
	return ConfigResponse{
		APIKey:       c.APIKey,
		Model:        c.Model,
		BaseURL:      c.BaseURL,
		DisableDelay: c.DisableDelay,
	}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, toConfigResponse(s.config))
}

type UpdateConfigRequest struct {
	APIKey       string `json:"apiKey"`
	Model        string `json:"model"`
	BaseURL      string `json:"baseUrl"`
	DisableDelay *bool  `json:"disableDelay"`
}

func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	var req UpdateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.APIKey != "" {
		s.config.APIKey = req.APIKey
	}
	if req.Model != "" {
		s.config.Model = req.Model
	}
	if req.BaseURL != "" {
		s.config.BaseURL = req.BaseURL
	}
	if req.DisableDelay != nil {
		s.config.DisableDelay = *req.DisableDelay
	}

	if err := db.UpdateConfig(s.config); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Update agent LLM client
	if s.agent != nil {
		llmClient := llm.NewClient(llm.Config{
			APIKey:  s.config.APIKey,
			BaseURL: s.config.BaseURL,
			Model:   s.config.Model,
		})
		s.agent.SetLLM(llmClient)
		s.agent.SetVision(llmClient)
	}

	writeJSON(w, toConfigResponse(s.config))
}

func (s *Server) unlinkMessenger(w http.ResponseWriter, r *http.Request) {
	mt := chi.URLParam(r, "type")
	messengerCfg := db.GetMessengerConfig(mt)
	if messengerCfg != nil {
		messengerCfg.Enabled = false
		if err := db.SaveMessengerConfig(messengerCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if msgr, ok := s.messengers[mt]; ok && msgr != nil {
			if err := msgr.Disconnect(); err != nil {
				slog.Error("Error disconnecting messenger", "error", err)
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
