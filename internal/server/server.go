// Package server exposes the app to a browser.
//
// It listens on loopback only. There is no authentication because there is no
// remote access: the socket is reachable from this machine and nothing else,
// which is the same trust boundary as the database file sitting next to it.
// Binding to a public interface would change that, so it is not offered.
package server

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/notshekhar/yap/internal/app"
	"github.com/notshekhar/yap/internal/identity"
	"github.com/notshekhar/yap/internal/store"
)

//go:embed web
var webFS embed.FS

// Server serves the chat UI and its API.
type Server struct {
	app *app.App
	log *slog.Logger
	mux *http.ServeMux
	srv *http.Server
}

// New builds a server.
func New(a *app.App, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{app: a, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Listen starts serving. It returns the address actually bound, which matters
// because the requested port may already be taken by another yap.
func (s *Server) Listen(port int) (string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Fall back to an ephemeral port rather than refusing to start: a
		// second node on one machine is exactly how you test this.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("listen: %w", err)
		}
	}

	s.srv = &http.Server{
		Handler:      s.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // the event stream is deliberately long-lived
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("http server stopped", "err", err)
		}
	}()
	return ln.Addr().String(), nil
}

// Close stops serving.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func (s *Server) routes() {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))

	s.mux.HandleFunc("GET /api/me", s.handleMe)
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("GET /api/messages", s.handleMessages)
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
	s.mux.HandleFunc("GET /blob/{id}", s.handleBlob)

	s.mux.HandleFunc("POST /api/send", s.handleSend)
	s.mux.HandleFunc("POST /api/contacts", s.handleAddContact)
	s.mux.HandleFunc("POST /api/chats", s.handleOpenChat)
	s.mux.HandleFunc("POST /api/read", s.handleRead)
	s.mux.HandleFunc("POST /api/typing", s.handleTyping)
	s.mux.HandleFunc("POST /api/profile", s.handleProfile)
	s.mux.HandleFunc("POST /api/delete", s.handleDelete)
	s.mux.HandleFunc("POST /api/chat-flag", s.handleChatFlag)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.Me())
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st := s.app.Store()

	chats, err := st.Chats()
	if err != nil {
		writeErr(w, err)
		return
	}
	contacts, err := st.Contacts()
	if err != nil {
		writeErr(w, err)
		return
	}

	nearby := s.app.Nearby()
	online := make(map[string]bool, len(nearby))
	for id := range nearby {
		online[id] = true
	}
	byID := make(map[string]*store.Contact, len(contacts))
	for _, c := range contacts {
		c.Online = online[c.NodeID]
		byID[c.NodeID] = c
	}
	for _, c := range chats {
		c.Online = online[c.ID]
		// A chat with no title yet shows the contact's name, and failing that
		// the short form of their address — never an empty row.
		if c.Title == "" {
			if contact := byID[c.ID]; contact != nil {
				c.Title = contact.Name
				if c.Title == "" {
					c.Title = contact.Key.Short()
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"me":       s.app.Me(),
		"chats":    chats,
		"contacts": contacts,
		"nearby":   nearby,
		"links":    s.app.Node().LinkCount(),
		"peers":    len(s.app.Node().Peers()),
		"pending":  s.app.Node().Pending(),
		"carrying": s.app.Node().Carrying(),
	})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	chat := r.URL.Query().Get("chat")
	if chat == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chat is required"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)

	msgs, err := s.app.Store().Messages(chat, limit, before)
	if err != nil {
		writeErr(w, err)
		return
	}
	if msgs == nil {
		msgs = []*store.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	mime, name, data, err := s.app.Store().Blob(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if data == nil {
		http.NotFound(w, r)
		return
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	// Attachments are user-supplied bytes rendered in a page that can reach
	// the local API. Refusing to sniff and refusing to frame keeps a crafted
	// "image" from being interpreted as something executable.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; sandbox")
	if name != "" {
		w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(name))
	}
	w.Write(data)
}

// handleEvents streams UI updates. Server-sent events rather than a websocket:
// the traffic is one-directional, and this needs no dependency and reconnects
// on its own.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, release := s.app.Subscribe()
	defer release()

	// A heartbeat keeps proxies and sleeping laptops from quietly dropping the
	// connection without the browser noticing.
	beat := time.NewTicker(25 * time.Second)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

type sendRequest struct {
	To      string `json:"to"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to"`

	// Attachment, base64 encoded by the browser.
	Kind string `json:"kind"`
	Mime string `json:"mime"`
	Name string `json:"name"`
	Data string `json:"data"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	key, err := s.resolve(req.To)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var msg *store.Message
	if req.Data != "" {
		raw, decErr := base64.StdEncoding.DecodeString(req.Data)
		if decErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attachment is not valid base64"})
			return
		}
		kind := req.Kind
		if kind == "" {
			kind = store.KindFile
		}
		msg, err = s.app.SendAttachment(key, kind, req.Mime, req.Name, raw, req.Body)
	} else {
		if strings.TrimSpace(req.Body) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is empty"})
			return
		}
		msg, err = s.app.SendText(key, req.Body, req.ReplyTo)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": msg})
}

// resolve accepts either a full address or the node id of a known contact,
// because the UI has the latter and a person pasting an invite has the former.
func (s *Server) resolve(to string) (identity.PublicKey, error) {
	var zero identity.PublicKey
	to = strings.TrimSpace(to)
	if to == "" {
		return zero, errors.New("recipient is required")
	}

	if c, err := s.app.Store().Contact(to); err == nil && c != nil {
		return c.Key, nil
	}
	key, err := identity.ParseAddress(to)
	if err != nil {
		return zero, fmt.Errorf("unknown recipient: %w", err)
	}
	return key, nil
}

func (s *Server) handleAddContact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	contact, err := s.app.AddContact(req.Address, req.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contact": contact})
}

// handleOpenChat gives a discovered peer a conversation to live in. The
// contact already exists — the radio heard them — so this only creates the
// thread the first time you decide to say something.
func (s *Server) handleOpenChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	contact, err := s.app.Store().Contact(req.Chat)
	if err != nil {
		writeErr(w, err)
		return
	}
	if contact == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such contact"})
		return
	}
	if err := s.app.Store().EnsureChat(contact.NodeID, "direct", contact.Name); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.app.MarkRead(req.Chat); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTyping(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
		On   bool   `json:"on"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if c, err := s.app.Store().Contact(req.Chat); err == nil && c != nil {
		s.app.SetTyping(c.Key, req.On)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.app.SetDisplayName(req.Name); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.app.Me())
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.app.DeleteForEveryone(req.ID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleChatFlag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat string `json:"chat"`
		Flag string `json:"flag"`
		On   bool   `json:"on"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.app.Store().SetChatFlag(req.Chat, req.Flag, req.On); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------------------

// maxBody bounds a request. Attachments are base64, so the cap is the
// attachment limit plus encoding overhead and a little slack for the envelope.
const maxBody = app.MaxAttachment*2 + 64*1024

func decode(r *http.Request, v any) error {
	body := http.MaxBytesReader(nil, r.Body, maxBody)
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("request too large or unreadable: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("malformed request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}
