// Package app is the chat application: it turns mesh traffic into
// conversations and conversations into mesh traffic.
//
// The mesh below deals in authenticated peers and opaque payloads. The store
// beside it deals in rows. This package owns the meaning in between — what a
// message is, what a receipt means, when a tick turns from one to two — and it
// is the only place that knows the payload format riding inside the
// encryption.
package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/notshekhar/yap/internal/identity"
	"github.com/notshekhar/yap/internal/mesh"
	"github.com/notshekhar/yap/internal/store"
)

// Payload kinds carried inside the encrypted envelope. A relay never sees
// these; only the two endpoints do.
const (
	kindMessage = "msg"
	kindReceipt = "receipt"
	kindTyping  = "typing"
	kindProfile = "profile"
	kindDelete  = "delete"
)

// MaxAttachment bounds what a single message may carry.
//
// Bluetooth moves a few kilobytes per second in good conditions. A megabyte
// photo would occupy the link for minutes, starve every other conversation
// sharing the radio, and very likely fail partway when someone walks off. The
// UI downscales images to fit this rather than refusing them.
const MaxAttachment = 96 * 1024

// envelope is the wire form of everything two peers say to each other.
type envelope struct {
	T       string `json:"t"`
	ID      string `json:"id,omitempty"`
	Body    string `json:"body,omitempty"`
	Kind    string `json:"kind,omitempty"`
	TS      int64  `json:"ts,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`

	// Attachment content, base64 encoded, for image and file messages.
	Mime string `json:"mime,omitempty"`
	Name string `json:"name,omitempty"`
	Data string `json:"data,omitempty"`

	// Receipts.
	IDs   []string `json:"ids,omitempty"`
	State string   `json:"state,omitempty"`

	// Typing and profile.
	On bool `json:"on,omitempty"`
}

// Event is something the UI should react to.
type Event struct {
	Type    string         `json:"type"`
	Chat    string         `json:"chat,omitempty"`
	Message *store.Message `json:"message,omitempty"`
	State   string         `json:"state,omitempty"`
	ID      string         `json:"id,omitempty"`
	Peer    string         `json:"peer,omitempty"`
	Name    string         `json:"name,omitempty"`
	On      bool           `json:"on,omitempty"`
}

// App is the running chat application.
type App struct {
	node  *mesh.Node
	store *store.Store
	log   *slog.Logger

	mu          sync.Mutex
	subscribers map[chan Event]struct{}

	// pending maps a mesh delivery reference back to the message it belongs
	// to, so an acknowledgement can advance the right row.
	pending map[string]string

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New builds the application over a running mesh node and an open store.
func New(node *mesh.Node, st *store.Store, log *slog.Logger) *App {
	if log == nil {
		log = slog.Default()
	}
	return &App{
		node:        node,
		store:       st,
		log:         log,
		subscribers: make(map[chan Event]struct{}),
		pending:     make(map[string]string),
		stop:        make(chan struct{}),
	}
}

// Start begins consuming mesh traffic and re-queues anything left over from
// the last run.
func (a *App) Start(ctx context.Context) {
	a.wg.Add(3)
	go a.consumeInbound()
	go a.consumeDeliveries()
	go a.watchPeers()

	// After the consumers, so a delivery receipt for a resumed message has
	// somewhere to land.
	a.resume()
}

// Close stops the application.
func (a *App) Close() error {
	a.stopOnce.Do(func() {
		close(a.stop)
		a.wg.Wait()
	})
	return nil
}

// Store exposes the database for the HTTP layer.
func (a *App) Store() *store.Store { return a.store }

// Node exposes the mesh for status reporting.
func (a *App) Node() *mesh.Node { return a.node }

// Me describes this node for the UI.
func (a *App) Me() map[string]any {
	return map[string]any{
		"address": a.node.Address(),
		"node_id": a.node.Identity().NodeID().String(),
		"short":   a.node.Identity().Public().Short(),
		"name":    a.DisplayName(),
	}
}

// DisplayName is the name this node announces.
func (a *App) DisplayName() string {
	return a.store.Setting("profile.name", "")
}

// SetDisplayName changes the announced name and tells current peers.
func (a *App) SetDisplayName(name string) error {
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		name = name[:64]
	}
	if err := a.store.SetSetting("profile.name", name); err != nil {
		return err
	}

	// Tell everyone we already have a session with, so the change shows up
	// without waiting for them to re-add us.
	contacts, err := a.store.Contacts()
	if err != nil {
		return err
	}
	for _, c := range contacts {
		a.sendEnvelope(c.Key, envelope{T: kindProfile, Body: name})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Subscriptions, for the server-sent event stream
// ---------------------------------------------------------------------------

// Subscribe returns a channel of UI events and a function to release it.
func (a *App) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)

	a.mu.Lock()
	a.subscribers[ch] = struct{}{}
	a.mu.Unlock()

	return ch, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if _, ok := a.subscribers[ch]; ok {
			delete(a.subscribers, ch)
			close(ch)
		}
	}
}

func (a *App) publish(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subscribers {
		select {
		case ch <- ev:
		default:
			// A browser tab that stopped reading must not stall the mesh.
		}
	}
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// SendText sends a text message to a contact.
func (a *App) SendText(to identity.PublicKey, body, replyTo string) (*store.Message, error) {
	return a.send(to, envelope{
		T:       kindMessage,
		Kind:    store.KindText,
		Body:    body,
		ReplyTo: replyTo,
	}, nil)
}

// SendAttachment sends an image or file.
func (a *App) SendAttachment(to identity.PublicKey, kind, mime, name string, data []byte, caption string) (*store.Message, error) {
	if len(data) > MaxAttachment {
		return nil, fmt.Errorf("attachment is %d bytes; the limit over Bluetooth is %d", len(data), MaxAttachment)
	}
	return a.send(to, envelope{
		T:    kindMessage,
		Kind: kind,
		Body: caption,
		Mime: mime,
		Name: name,
		Data: base64.StdEncoding.EncodeToString(data),
	}, data)
}

func (a *App) send(to identity.PublicKey, env envelope, blob []byte) (*store.Message, error) {
	if to == a.node.Identity().Public() {
		return nil, errors.New("that address is your own")
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	env.ID = id
	env.TS = now

	chatID := to.NodeID().String()
	if err := a.store.EnsureChat(chatID, "direct", ""); err != nil {
		return nil, err
	}

	blobID := ""
	if len(blob) > 0 {
		blobID = id
		if err := a.store.PutBlob(blobID, env.Mime, env.Name, blob); err != nil {
			return nil, err
		}
	}

	msg := &store.Message{
		ID:         id,
		ChatID:     chatID,
		Author:     a.node.Identity().NodeID().String(),
		Mine:       true,
		Kind:       env.Kind,
		Body:       env.Body,
		BlobID:     blobID,
		ReplyTo:    env.ReplyTo,
		CreatedAt:  now,
		ReceivedAt: now,
		State:      store.StateQueued,
	}
	if _, err := a.store.AddMessage(msg); err != nil {
		return nil, err
	}

	// The message is durable before it is transmitted, so a crash between the
	// two loses nothing: resume() picks it up on the next start.
	a.publish(Event{Type: "message", Chat: chatID, Message: msg})

	if err := a.transmit(to, env, id, chatID); err != nil {
		return msg, err
	}
	msg.State = store.StateSent
	return msg, nil
}

// transmit puts one message on the mesh and registers it for delivery
// tracking. Both a fresh send and a resume after restart go through here, so
// there is one place that decides what "sent" means.
func (a *App) transmit(to identity.PublicKey, env envelope, msgID, chatID string) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}

	ref := "m:" + msgID
	a.mu.Lock()
	a.pending[ref] = msgID
	a.mu.Unlock()

	if err := a.node.Send(to, payload, ref); err != nil {
		// Drop the tracking entry too, or a message that never left holds a
		// slot in the map for the life of the process.
		a.mu.Lock()
		delete(a.pending, ref)
		a.mu.Unlock()

		a.setState(msgID, chatID, store.StateFailed)
		return err
	}
	a.setState(msgID, chatID, store.StateSent)
	return nil
}

// envelopeFor rebuilds the wire form of a stored message, reloading any
// attachment from the blob store.
func (a *App) envelopeFor(m *store.Message) (envelope, error) {
	env := envelope{
		T:       kindMessage,
		ID:      m.ID,
		Kind:    m.Kind,
		Body:    m.Body,
		TS:      m.CreatedAt,
		ReplyTo: m.ReplyTo,
	}
	if m.BlobID == "" {
		return env, nil
	}

	mime, name, data, err := a.store.Blob(m.BlobID)
	if err != nil {
		return env, err
	}
	if data == nil {
		// The row survived but its attachment did not. Send the caption rather
		// than nothing, so the conversation is not silently missing a turn.
		a.log.Warn("attachment is missing, sending without it", "message", m.ID)
		return env, nil
	}
	env.Mime, env.Name = mime, name
	env.Data = base64.StdEncoding.EncodeToString(data)
	return env, nil
}

// resume re-queues everything that was still waiting when we last stopped.
//
// Without this the product's central promise quietly fails: a message to
// someone out of range is durable in the database, but nothing would ever put
// it back on the air, so quitting yap would abandon it. Message ids are
// preserved, so a recipient who did receive it the first time discards the
// repeat as a duplicate.
func (a *App) resume() {
	msgs, err := a.store.Undelivered()
	if err != nil {
		a.log.Error("could not read the outbox", "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}

	resumed := 0
	for _, m := range msgs {
		contact, err := a.store.Contact(m.ChatID)
		if err != nil || contact == nil {
			// Nothing to address it to. Mark it failed rather than leaving it
			// to be reconsidered on every start for ever.
			a.setState(m.ID, m.ChatID, store.StateFailed)
			continue
		}

		env, err := a.envelopeFor(m)
		if err != nil {
			a.log.Error("could not rebuild a queued message", "message", m.ID, "err", err)
			continue
		}
		if err := a.transmit(contact.Key, env, m.ID, m.ChatID); err != nil {
			a.log.Debug("queued message still cannot go out", "message", m.ID, "err", err)
			continue
		}
		resumed++
	}

	if resumed > 0 {
		a.log.Info("put queued messages back on the air", "count", resumed)
	}
}

// sendEnvelope sends a control message, which has no local record.
func (a *App) sendEnvelope(to identity.PublicKey, env envelope) {
	payload, err := json.Marshal(env)
	if err != nil {
		return
	}
	if err := a.node.Send(to, payload, "ctl"); err != nil {
		a.log.Debug("could not send control message", "kind", env.T, "err", err)
	}
}

// SetTyping tells a peer whether we are composing.
func (a *App) SetTyping(to identity.PublicKey, on bool) {
	a.sendEnvelope(to, envelope{T: kindTyping, On: on})
}

// MarkRead marks a conversation read and tells the sender.
func (a *App) MarkRead(chatID string) error {
	ids, err := a.store.MarkChatRead(chatID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	contact, err := a.store.Contact(chatID)
	if err != nil || contact == nil {
		return err
	}
	a.sendEnvelope(contact.Key, envelope{T: kindReceipt, IDs: ids, State: store.StateRead})
	a.publish(Event{Type: "chat", Chat: chatID})
	return nil
}

// DeleteForEveryone asks the other side to drop a message.
//
// It is a request, not a command. The recipient's node honours it because it
// chooses to; nothing in the protocol could compel a peer that has already
// received the bytes, and the UI is written to say so.
func (a *App) DeleteForEveryone(messageID string) error {
	msg, err := a.store.Message(messageID)
	if err != nil || msg == nil {
		return err
	}
	if err := a.store.DeleteMessage(messageID); err != nil {
		return err
	}
	a.publish(Event{Type: "deleted", Chat: msg.ChatID, ID: messageID})

	contact, err := a.store.Contact(msg.ChatID)
	if err != nil || contact == nil {
		return err
	}
	a.sendEnvelope(contact.Key, envelope{T: kindDelete, IDs: []string{messageID}})
	return nil
}

// AddContact records a peer by address so they can be messaged.
func (a *App) AddContact(address, name string) (*store.Contact, error) {
	key, err := identity.ParseAddress(address)
	if err != nil {
		return nil, err
	}
	if key == a.node.Identity().Public() {
		return nil, errors.New("that address is your own")
	}
	if err := a.store.SaveContact(key, name); err != nil {
		return nil, err
	}
	chatID := key.NodeID().String()
	if err := a.store.EnsureChat(chatID, "direct", name); err != nil {
		return nil, err
	}

	a.publish(Event{Type: "contacts"})
	return a.store.Contact(chatID)
}

// ---------------------------------------------------------------------------
// Receiving
// ---------------------------------------------------------------------------

func (a *App) consumeInbound() {
	defer a.wg.Done()
	for {
		select {
		case <-a.stop:
			return
		case in, ok := <-a.node.Inbound():
			if !ok {
				return
			}
			a.handleInbound(in)
		}
	}
}

func (a *App) handleInbound(in mesh.Inbound) {
	var env envelope
	if err := json.Unmarshal(in.Payload, &env); err != nil {
		a.log.Debug("undecodable payload", "from", in.From.NodeID(), "err", err)
		return
	}

	chatID := in.From.NodeID().String()

	// A message from someone unknown still has a proven key: the session could
	// not have been established otherwise. Record them so the conversation has
	// somewhere to live, but leave them unnamed until they say who they are.
	contact, err := a.store.Contact(chatID)
	if err != nil {
		a.log.Error("contact lookup failed", "err", err)
		return
	}
	if contact == nil {
		if err := a.store.SaveContact(in.From, ""); err != nil {
			a.log.Error("could not record new contact", "err", err)
			return
		}
		a.store.EnsureChat(chatID, "direct", "")
		a.publish(Event{Type: "contacts"})
	} else {
		a.store.TouchContact(in.From)
		if contact.Blocked {
			return
		}
	}

	switch env.T {
	case kindMessage:
		a.receiveMessage(in.From, chatID, env)
	case kindReceipt:
		a.receiveReceipt(chatID, env)
	case kindTyping:
		a.publish(Event{Type: "typing", Chat: chatID, On: env.On})
	case kindProfile:
		a.store.SaveContact(in.From, env.Body)
		a.store.EnsureChat(chatID, "direct", env.Body)
		a.publish(Event{Type: "contacts"})
	case kindDelete:
		for _, id := range env.IDs {
			if m, _ := a.store.Message(id); m != nil && m.ChatID == chatID && !m.Mine {
				a.store.DeleteMessage(id)
				a.publish(Event{Type: "deleted", Chat: chatID, ID: id})
			}
		}
	default:
		a.log.Debug("unknown payload kind", "kind", env.T)
	}
}

func (a *App) receiveMessage(from identity.PublicKey, chatID string, env envelope) {
	if env.ID == "" {
		return
	}
	now := time.Now().UnixMilli()
	created := env.TS
	if created <= 0 {
		created = now
	}

	blobID := ""
	if env.Data != "" {
		data, err := base64.StdEncoding.DecodeString(env.Data)
		if err != nil {
			a.log.Debug("bad attachment encoding", "err", err)
		} else if len(data) > MaxAttachment {
			a.log.Warn("oversized attachment refused", "bytes", len(data))
		} else {
			blobID = env.ID
			if err := a.store.PutBlob(blobID, env.Mime, env.Name, data); err != nil {
				a.log.Error("could not store attachment", "err", err)
				blobID = ""
			}
		}
	}

	kind := env.Kind
	if kind == "" {
		kind = store.KindText
	}

	msg := &store.Message{
		ID:         env.ID,
		ChatID:     chatID,
		Author:     chatID,
		Mine:       false,
		Kind:       kind,
		Body:       env.Body,
		BlobID:     blobID,
		ReplyTo:    env.ReplyTo,
		CreatedAt:  created,
		ReceivedAt: now,
		State:      store.StateDelivered,
	}

	fresh, err := a.store.AddMessage(msg)
	if err != nil {
		a.log.Error("could not store message", "err", err)
		return
	}
	if !fresh {
		// The mesh delivered a duplicate, which is normal. The user already
		// has it, so do not announce it twice.
		return
	}

	a.publish(Event{Type: "message", Chat: chatID, Message: msg})

	// Confirm delivery so the sender's second tick appears.
	a.sendEnvelope(from, envelope{T: kindReceipt, IDs: []string{env.ID}, State: store.StateDelivered})
}

func (a *App) receiveReceipt(chatID string, env envelope) {
	state := env.State
	if _, ok := map[string]bool{
		store.StateDelivered: true,
		store.StateRead:      true,
	}[state]; !ok {
		return
	}
	for _, id := range env.IDs {
		a.setState(id, chatID, state)
	}
}

func (a *App) setState(id, chatID, state string) {
	if err := a.store.SetState(id, state); err != nil {
		a.log.Debug("could not update message state", "id", id, "err", err)
		return
	}
	a.publish(Event{Type: "state", Chat: chatID, ID: id, State: state})
}

// consumeDeliveries turns mesh acknowledgements into ticks.
func (a *App) consumeDeliveries() {
	defer a.wg.Done()
	for {
		select {
		case <-a.stop:
			return
		case d, ok := <-a.node.Delivered():
			if !ok {
				return
			}
			a.mu.Lock()
			msgID, known := a.pending[d.Ref]
			if known {
				delete(a.pending, d.Ref)
			}
			a.mu.Unlock()
			if !known {
				continue
			}

			msg, err := a.store.Message(msgID)
			if err != nil || msg == nil {
				continue
			}
			if d.Err != nil {
				a.setState(msgID, msg.ChatID, store.StateFailed)
				continue
			}
			if d.Acked {
				a.setState(msgID, msg.ChatID, store.StateDelivered)
			}
		}
	}
}

// watchPeers keeps the contact list's presence indicators current.
func (a *App) watchPeers() {
	defer a.wg.Done()

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	prev := ""
	for {
		select {
		case <-a.stop:
			return
		case <-tick.C:
			peers := a.node.Peers()

			// Anyone the radio can hear becomes a contact automatically. This
			// is the point of a proximity messenger: you should not have to
			// type an address for somebody standing next to you, and their
			// announce already carries everything needed to reach them.
			//
			// The announce is unauthenticated, so a hostile node can claim a
			// key it does not hold and appear in this list. That costs it
			// nothing and gains it nothing: anything sent to that key is
			// encrypted to the real holder, and the impostor cannot complete a
			// handshake for it. A fake row is the whole of the attack.
			//
			// No chat row is created here. Being in the room is not a
			// conversation, and manufacturing empty threads for everyone who
			// walks past would bury the real ones.
			var ids []string
			for _, p := range peers {
				ids = append(ids, p.NodeID().String())
				if err := a.store.SaveContact(p.Key, p.Name); err != nil {
					a.log.Debug("could not record a nearby peer", "err", err)
				}
			}

			// Only wake the UI when the set actually changed.
			key := strings.Join(ids, ",")
			if key != prev {
				prev = key
				a.publish(Event{Type: "peers"})
			}
		}
	}
}

// Nearby maps each reachable peer to how many relays away it is: 0 means we
// can hear them directly. The UI turns this into the range meter, which is the
// one thing a messenger over radio knows and a messenger over the internet
// does not.
func (a *App) Nearby() map[string]int {
	out := make(map[string]int)
	for _, p := range a.node.Peers() {
		hops := p.Hops
		if p.Direct {
			hops = 0
		}
		out[p.NodeID().String()] = hops
	}
	return out
}

// newID draws a random message identifier.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("draw message id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
