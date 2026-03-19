package api

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/julienrbrt/talktothem/internal/agent"
	"github.com/julienrbrt/talktothem/internal/contact"
	"github.com/julienrbrt/talktothem/internal/conversation"
	"github.com/julienrbrt/talktothem/internal/db"
	"github.com/julienrbrt/talktothem/internal/messenger"
	signalcli "github.com/julienrbrt/talktothem/internal/messenger/signal"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type Server struct {
	server       *http.Server
	router       *chi.Mux
	agent        *agent.Agent
	contacts     *contact.Manager
	messenger    messenger.Messenger
	signalClient *signalcli.Client
	config       *db.Config
	hub          *Hub
	templates    *template.Template
	assets       fs.FS
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
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

//go:embed templates/*.html templates/partials/*.html
var templatesFS embed.FS

func NewServer(addr string, ag *agent.Agent, cm *contact.Manager, m messenger.Messenger, cfg *db.Config, assets fs.FS, signalAPIURL string) *Server {
	r := chi.NewRouter()

	tmpl := template.Must(template.ParseFS(templatesFS,
		"templates/base.html",
		"templates/index.html",
		"templates/partials/*.html",
	))

	s := &Server{
		router:       r,
		agent:        ag,
		contacts:     cm,
		messenger:    m,
		signalClient: signalcli.NewWithoutNumber(signalAPIURL),
		config:       cfg,
		hub:          NewHub(),
		templates:    tmpl,
		assets:       assets,
	}

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	r.Route("/api", func(r chi.Router) {
		// Onboarding
		r.Get("/status", s.getStatus)
		r.Post("/onboarding", s.completeOnboarding)

		// Signal device linking
		r.Get("/signal/link/start", s.startSignalLinking)
		r.Get("/signal/link/status", s.getSignalLinkStatus)

		// Contacts
		r.Get("/contacts", s.listContacts)
		r.Get("/contacts/all", s.listAllContacts)
		r.Post("/contacts", s.createContact)
		r.Post("/contacts/import", s.importContactsFromMessenger)
		r.Post("/contacts/upload-vcf", s.uploadVCF)
		r.Get("/contacts/{id}", s.getContact)
		r.Put("/contacts/{id}", s.updateContact)
		r.Delete("/contacts/{id}", s.deleteContact)
		r.Post("/contacts/{id}/enable", s.enableContact)
		r.Post("/contacts/{id}/disable", s.disableContact)
		r.Get("/contacts/{id}/conversation", s.getConversation)
		r.Post("/contacts/{id}/message", s.sendMessage)
		r.Post("/contacts/{id}/sync", s.syncConversation)
		r.Post("/contacts/{id}/learn-style", s.learnStyle)
		r.Post("/contacts/{id}/initiate", s.initiateConversation)
		r.Get("/contacts/{id}/response-check", s.checkResponse)

		// User Profile
		r.Get("/profile", s.getUserProfile)
		r.Put("/profile", s.updateUserProfile)
		r.Post("/profile/learn-style", s.learnUserStyle)
	})

	r.Get("/ws", s.handleWebSocket)

	r.Get("/", s.indexPage)
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
	sub, err := fs.Sub(root, "dist")
	if err != nil {
		return
	}
	fs := http.FileServer(http.FS(sub))

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

func (s *Server) Run() error {
	go s.hub.Run()
	if s.agent != nil {
		go s.listenForAgentResponses()
	}
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *Server) listenForAgentResponses() {
	for resp := range s.agent.Outbox() {
		event := MessageEvent{
			Type: "agent_response",
			Payload: map[string]string{
				"contactId": resp.ContactID,
				"content":   resp.Content,
			},
		}
		data, _ := json.Marshal(event)
		s.hub.broadcast <- data
	}
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

func (s *Server) broadcastEvent(eventType string, payload interface{}) {
	event := MessageEvent{
		Type:    eventType,
		Payload: payload,
	}
	data, _ := json.Marshal(event)
	s.hub.broadcast <- data
}

type ContactResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	Messenger   string `json:"messenger"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
	Style       string `json:"style"`
}

func contactToResponse(c contact.Contact) ContactResponse {
	return ContactResponse{
		ID:          c.ID,
		Name:        c.Name,
		Phone:       c.Phone,
		Messenger:   c.Messenger,
		Enabled:     c.Enabled,
		Description: c.Description,
		Style:       c.Style,
	}
}

type MessageResponse struct {
	ID        string    `json:"id"`
	ContactID string    `json:"contactId"`
	Content   string    `json:"content"`
	Type      string    `json:"type"`
	MediaURL  string    `json:"mediaUrl,omitempty"`
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
		MediaURL:  m.MediaURL,
		Timestamp: m.Timestamp,
		IsFromMe:  m.IsFromMe,
		Reaction:  m.Reaction,
	}
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request) {
	contacts := s.contacts.ListEnabled()

	var response []ContactResponse
	for _, c := range contacts {
		response = append(response, contactToResponse(c))
	}

	if response == nil {
		response = []ContactResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "contacts", response)
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type CreateContactRequest struct {
	Name        string
	Phone       string
	Description string
}

func (s *Server) createContact(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req := CreateContactRequest{
		Name:        r.FormValue("name"),
		Phone:       r.FormValue("phone"),
		Description: r.FormValue("description"),
	}

	c := contact.Contact{
		ID:          req.Phone,
		Name:        req.Name,
		Phone:       req.Phone,
		Description: req.Description,
		Enabled:     false,
	}

	if err := s.contacts.Add(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return updated contact list for HTMX
	contacts := s.contacts.List()
	var response []ContactResponse
	for _, ct := range contacts {
		response = append(response, contactToResponse(ct))
	}
	if response == nil {
		response = []ContactResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "contacts", response)
}

func (s *Server) getContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	c, ok := s.contacts.Get(id)
	if !ok {
		http.Error(w, "contact not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(contactToResponse(c))
}

type UpdateContactRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
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

	if err := s.contacts.Add(c); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.broadcastEvent("contact_updated", contactToResponse(c))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(contactToResponse(c))
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

	c, _ := s.contacts.Get(id)

	// Check for unanswered messages
	check, _ := s.agent.CheckResponse(id, 48*time.Hour)

	response := map[string]interface{}{
		"contact":         contactToResponse(c),
		"hasUnanswered":   check.Needed,
		"lastSender":      check.LastSender,
		"lastMessageTime": check.LastAt,
	}

	s.broadcastEvent("contact_enabled", contactToResponse(c))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *Server) disableContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := s.contacts.SetEnabled(id, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	c, _ := s.contacts.Get(id)
	s.broadcastEvent("contact_disabled", contactToResponse(c))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(contactToResponse(c))
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
	s.templates.ExecuteTemplate(w, "messages", response)
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

	if s.messenger == nil {
		http.Error(w, "messenger not connected", http.StatusServiceUnavailable)
		return
	}

	if err := s.messenger.SendMessage(r.Context(), id, req.Content); err != nil {
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messageToResponse(msg))
}

func (s *Server) syncConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if s.messenger == nil {
		http.Error(w, "messenger not connected", http.StatusServiceUnavailable)
		return
	}

	if err := s.agent.SyncHistory(r.Context(), s.messenger, id); err != nil {
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"style": style})
}

func (s *Server) initiateConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	msg, err := s.agent.Initiate(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s.messenger != nil && msg != "" {
		if err := s.messenger.SendMessage(r.Context(), id, msg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

type ResponseCheckResponse struct {
	Needed     bool      `json:"needed"`
	LastSender string    `json:"lastSender"`
	LastAt     time.Time `json:"lastAt"`
}

func (s *Server) checkResponse(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	check, err := s.agent.CheckResponse(id, 24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ResponseCheckResponse{
		Needed:     check.Needed,
		LastSender: check.LastSender,
		LastAt:     check.LastAt,
	})
}

type StatusResponse struct {
	Onboarded      bool   `json:"onboarded"`
	HasSignal      bool   `json:"hasSignal"`
	HasAPIKey      bool   `json:"hasApiKey"`
	SignalNumber   string `json:"signalNumber,omitempty"`
	ConnectedCount int    `json:"connectedCount"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	hasAPIKey := s.config.APIKey != ""
	signalCfg := db.GetMessengerConfig("signal")
	hasSignal := signalCfg != nil && signalCfg.Enabled && signalCfg.Phone != "" && s.messenger != nil
	contacts := s.contacts.List()
	var connected int
	for _, c := range contacts {
		if c.Enabled {
			connected++
		}
	}

	var signalNumber string
	if signalCfg != nil {
		signalNumber = signalCfg.Phone
	}

	response := StatusResponse{
		Onboarded:      hasAPIKey && hasSignal,
		HasSignal:      hasSignal,
		HasAPIKey:      hasAPIKey,
		SignalNumber:   signalNumber,
		ConnectedCount: connected,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type SignalLinkResponse struct {
	QRCode string `json:"qrCode"` // base64 encoded PNG
}

func (s *Server) startSignalLinking(w http.ResponseWriter, r *http.Request) {
	qrCode, err := s.signalClient.StartLinking(r.Context(), "TalkToThem")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := SignalLinkResponse{
		QRCode: base64.StdEncoding.EncodeToString(qrCode),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type SignalLinkStatusResponse struct {
	Linked bool   `json:"linked"`
	Number string `json:"number,omitempty"`
}

func (s *Server) getSignalLinkStatus(w http.ResponseWriter, r *http.Request) {
	linked, number, err := s.signalClient.IsLinked(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := SignalLinkStatusResponse{
		Linked: linked,
		Number: number,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type OnboardingRequest struct {
	APIKey  string
	Model   string
	BaseURL string
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
	}

	// Get the number from the linked Signal device
	linked, number, err := s.signalClient.IsLinked(r.Context())
	if err != nil {
		http.Error(w, "failed to check signal link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !linked {
		http.Error(w, "signal device not linked", http.StatusBadRequest)
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

	// Save Signal messenger config with number from linked device
	signalCfg := &db.MessengerConfig{
		Type:    "signal",
		Phone:   number,
		Enabled: true,
	}
	if err := db.SaveMessengerConfig(signalCfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

type MessengerContact struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

func (s *Server) importContactsFromMessenger(w http.ResponseWriter, r *http.Request) {
	// Get number from linked device
	linked, number, err := s.signalClient.IsLinked(r.Context())
	if err != nil {
		http.Error(w, "failed to check signal link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !linked {
		http.Error(w, "signal device not linked", http.StatusBadRequest)
		return
	}

	// Create temporary client with the linked number to fetch contacts
	tempClient := signalcli.New(number, s.signalClient.GetBaseURL())

	messengerContacts, err := tempClient.GetContacts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var imported []MessengerContact
	for _, mc := range messengerContacts {
		if mc.Phone == "" {
			continue
		}

		existing, _ := s.contacts.Get(mc.Phone)
		if existing.ID != "" {
			continue
		}

		c := contact.Contact{
			ID:      mc.Phone,
			Name:    mc.Name,
			Phone:   mc.Phone,
			Enabled: false,
		}

		if err := s.contacts.Add(c); err != nil {
			continue
		}

		imported = append(imported, MessengerContact{
			ID:    c.ID,
			Name:  c.Name,
			Phone: c.Phone,
		})
	}

	// Return updated contact list for HTMX
	contacts := s.contacts.List()
	var response []ContactResponse
	for _, ct := range contacts {
		response = append(response, contactToResponse(ct))
	}
	if response == nil {
		response = []ContactResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "contacts", response)
}

func (s *Server) uploadVCF(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "file too large", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	importedContacts, err := contact.ParseVCARD(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, c := range importedContacts {
		if c.Phone == "" {
			continue
		}

		existing, _ := s.contacts.Get(c.Phone)
		if existing.ID != "" {
			continue
		}

		if c.ID == "" {
			c.ID = c.Phone
		}

		if err := s.contacts.Add(c); err != nil {
			continue
		}
	}

	// Return updated contact list for HTMX
	contacts := s.contacts.List()
	var response []ContactResponse
	for _, ct := range contacts {
		response = append(response, contactToResponse(ct))
	}
	if response == nil {
		response = []ContactResponse{}
	}

	w.Header().Set("Content-Type", "text/html")
	s.templates.ExecuteTemplate(w, "contacts", response)
}

type IndexData struct {
	Onboarded bool
	HasSignal bool
}

func (s *Server) indexPage(w http.ResponseWriter, r *http.Request) {
	hasAPIKey := s.config.APIKey != ""
	signalCfg := db.GetMessengerConfig("signal")
	hasSignalConfig := signalCfg != nil && signalCfg.Enabled && signalCfg.Phone != ""
	hasSignal := hasSignalConfig && s.messenger != nil

	data := IndexData{
		Onboarded: hasAPIKey && hasSignalConfig,
		HasSignal: hasSignal,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type ConversationData struct {
	Contact   contact.Contact
	Messages  []MessageResponse
	HasSignal bool
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

	signalCfg := db.GetMessengerConfig("signal")
	hasSignal := signalCfg != nil && signalCfg.Enabled && signalCfg.Phone != "" && s.messenger != nil

	data := ConversationData{
		Contact:   c,
		Messages:  msgResponses,
		HasSignal: hasSignal,
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "conversation-page", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type UserProfileResponse struct {
	Name          string `json:"name"`
	About         string `json:"about"`
	FamilyContext string `json:"familyContext"`
	WorkContext   string `json:"workContext"`
	WritingStyle  string `json:"writingStyle"`
}

func (s *Server) getUserProfile(w http.ResponseWriter, r *http.Request) {
	profile := db.GetUserProfile()

	response := UserProfileResponse{
		Name:          profile.Name,
		About:         profile.About,
		FamilyContext: profile.FamilyContext,
		WorkContext:   profile.WorkContext,
		WritingStyle:  profile.WritingStyle,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type UpdateUserProfileRequest struct {
	Name          string `json:"name"`
	About         string `json:"about"`
	FamilyContext string `json:"familyContext"`
	WorkContext   string `json:"workContext"`
	WritingStyle  string `json:"writingStyle"`
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

	if err := db.UpdateUserProfile(profile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(UserProfileResponse{
		Name:          profile.Name,
		About:         profile.About,
		FamilyContext: profile.FamilyContext,
		WorkContext:   profile.WorkContext,
		WritingStyle:  profile.WritingStyle,
	})
}

func (s *Server) learnUserStyle(w http.ResponseWriter, r *http.Request) {
	style, err := s.agent.LearnUserStyle(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	profile := db.GetUserProfile()
	profile.WritingStyle = style
	if err := db.UpdateUserProfile(profile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"style": style})
}
