package app

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/notshekhar/yap/internal/identity"
	"github.com/notshekhar/yap/internal/mesh"
	"github.com/notshekhar/yap/internal/store"
	"github.com/notshekhar/yap/internal/transport/loopback"
)

// A pair of running apps in one virtual room, which is as close to the real
// thing as a test can get without a radio.
type pair struct {
	t          *testing.T
	room       *loopback.Room
	alice, bob *node
}

type node struct {
	name  string
	id    *identity.Identity
	store *store.Store
	mesh  *mesh.Node
	app   *App
	dir   string
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newNode(t *testing.T, room *loopback.Room, name, dir string, id *identity.Identity) *node {
	t.Helper()

	if id == nil {
		var err error
		id, err = identity.New()
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dir, "yap.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := mesh.New(mesh.Config{
		Identity:  id,
		Name:      name,
		Transport: room.Join(name),
		Logger:    quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	a := New(m, st, quietLog())
	a.SetDisplayName(name)
	a.Start(context.Background())

	n := &node{name: name, id: id, store: st, mesh: m, app: a, dir: dir}
	t.Cleanup(func() { n.close() })
	return n
}

func (n *node) close() {
	n.app.Close()
	n.mesh.Close()
	n.store.Close()
}

func newPair(t *testing.T) *pair {
	t.Helper()
	room := loopback.NewRoom()
	p := &pair{
		t:     t,
		room:  room,
		alice: newNode(t, room, "alice", t.TempDir(), nil),
		bob:   newNode(t, room, "bob", t.TempDir(), nil),
	}
	room.Connect("alice", "bob")
	return p
}

// waitFor polls until a condition holds. Everything here crosses a mesh, so
// nothing is synchronous.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func (n *node) messages(chatID string) []*store.Message {
	msgs, err := n.store.Messages(chatID, 200, 0)
	if err != nil {
		return nil
	}
	return msgs
}

func TestTextMessageArrivesAndIsAttributed(t *testing.T) {
	p := newPair(t)

	if _, err := p.alice.app.SendText(p.bob.id.Public(), "are you nearby?", ""); err != nil {
		t.Fatal(err)
	}

	aliceChat := p.alice.id.NodeID().String()
	waitFor(t, "bob to receive the message", func() bool {
		return len(p.bob.messages(aliceChat)) == 1
	})

	got := p.bob.messages(aliceChat)[0]
	if got.Body != "are you nearby?" {
		t.Fatalf("body is %q", got.Body)
	}
	if got.Mine {
		t.Fatal("an incoming message was recorded as mine")
	}
	if got.State != store.StateDelivered {
		t.Fatalf("state is %q", got.State)
	}
}

// The two ticks: the sender learns their message landed.
func TestDeliveryReceiptReachesTheSender(t *testing.T) {
	p := newPair(t)

	msg, err := p.alice.app.SendText(p.bob.id.Public(), "hello", "")
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "alice to see the delivery receipt", func() bool {
		m, _ := p.alice.store.Message(msg.ID)
		return m != nil && m.State == store.StateDelivered
	})
}

func TestReadReceiptReachesTheSender(t *testing.T) {
	p := newPair(t)

	msg, err := p.alice.app.SendText(p.bob.id.Public(), "read this", "")
	if err != nil {
		t.Fatal(err)
	}

	aliceChat := p.alice.id.NodeID().String()
	waitFor(t, "bob to receive it", func() bool {
		return len(p.bob.messages(aliceChat)) == 1
	})

	if err := p.bob.app.MarkRead(aliceChat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice to see it was read", func() bool {
		m, _ := p.alice.store.Message(msg.ID)
		return m != nil && m.State == store.StateRead
	})
}

// A stranger's key is proven by the handshake, so a message from someone we
// have never added still lands and creates a conversation.
func TestMessageFromAnUnknownPeerCreatesAContact(t *testing.T) {
	p := newPair(t)

	// Bob has never heard of alice.
	if c, _ := p.bob.store.Contact(p.alice.id.NodeID().String()); c != nil {
		t.Fatal("bob already knows alice")
	}

	if _, err := p.alice.app.SendText(p.bob.id.Public(), "hi, you don't know me", ""); err != nil {
		t.Fatal(err)
	}

	aliceChat := p.alice.id.NodeID().String()
	waitFor(t, "bob to record the stranger", func() bool {
		c, _ := p.bob.store.Contact(aliceChat)
		return c != nil
	})

	c, _ := p.bob.store.Contact(aliceChat)
	if c.Key != p.alice.id.Public() {
		t.Fatal("the recorded key is not the sender's")
	}
}

// The name a peer announces should fill itself in without anyone typing it.
func TestDisplayNamePropagates(t *testing.T) {
	p := newPair(t)

	if _, err := p.alice.app.SendText(p.bob.id.Public(), "hello", ""); err != nil {
		t.Fatal(err)
	}
	aliceChat := p.alice.id.NodeID().String()

	waitFor(t, "bob to learn alice's name", func() bool {
		c, _ := p.bob.store.Contact(aliceChat)
		return c != nil && c.Name == "alice"
	})
}

// A proximity messenger should not ask you to type the address of somebody
// standing next to you. Presence announcements alone must be enough to make
// them reachable.
func TestNearbyPeersAreDiscoveredWithoutBeingAdded(t *testing.T) {
	p := newPair(t)

	bobID := p.bob.id.NodeID().String()
	waitFor(t, "alice to discover bob by herself", func() bool {
		c, _ := p.alice.store.Contact(bobID)
		return c != nil
	})

	c, _ := p.alice.store.Contact(bobID)
	if c.Key != p.bob.id.Public() {
		t.Fatal("the discovered key is not bob's")
	}
	if c.Name != "bob" {
		t.Fatalf("the announced name did not arrive: %q", c.Name)
	}

	// Discovery alone must not manufacture a conversation, or a busy room
	// would bury the real threads under everyone who walked past.
	chats, err := p.alice.store.Chats()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 0 {
		t.Fatalf("discovery created %d empty chats", len(chats))
	}

	// And discovery is enough to message them: no address was ever typed.
	if _, err := p.alice.app.SendText(c.Key, "saw you were nearby", ""); err != nil {
		t.Fatal(err)
	}
	aliceChat := p.alice.id.NodeID().String()
	waitFor(t, "the message to reach a peer nobody added by hand", func() bool {
		return len(p.bob.messages(aliceChat)) == 1
	})
}

func TestAttachmentSurvivesTheTrip(t *testing.T) {
	p := newPair(t)

	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 251)
	}

	if _, err := p.alice.app.SendAttachment(p.bob.id.Public(), store.KindImage,
		"image/jpeg", "photo.jpg", data, "look at this"); err != nil {
		t.Fatal(err)
	}

	aliceChat := p.alice.id.NodeID().String()
	waitFor(t, "bob to receive the attachment", func() bool {
		return len(p.bob.messages(aliceChat)) == 1
	})

	got := p.bob.messages(aliceChat)[0]
	if got.Kind != store.KindImage || got.Body != "look at this" {
		t.Fatalf("message came through as %+v", got)
	}
	if got.BlobID == "" {
		t.Fatal("no attachment was stored")
	}

	mime, name, blob, err := p.bob.store.Blob(got.BlobID)
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" || name != "photo.jpg" {
		t.Fatalf("attachment metadata is %q %q", mime, name)
	}
	if len(blob) != len(data) {
		t.Fatalf("attachment is %d bytes, sent %d", len(blob), len(data))
	}
	for i := range data {
		if blob[i] != data[i] {
			t.Fatalf("attachment differs at byte %d", i)
		}
	}
}

// Bluetooth cannot carry a large file in any reasonable time, so the limit is
// refused at the door rather than failing halfway.
func TestOversizedAttachmentRefused(t *testing.T) {
	p := newPair(t)

	_, err := p.alice.app.SendAttachment(p.bob.id.Public(), store.KindFile,
		"application/pdf", "big.pdf", make([]byte, MaxAttachment+1), "")
	if err == nil {
		t.Fatal("an oversized attachment was accepted")
	}
}

// The whole product promise: a message to somebody out of range waits, and
// goes out when they appear. This must survive the app being restarted, which
// is where it was silently broken.
func TestQueuedMessageResumesAfterRestart(t *testing.T) {
	room := loopback.NewRoom()

	aliceDir := t.TempDir()
	aliceID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	bob := newNode(t, room, "bob", t.TempDir(), nil)

	// Alice writes to bob while nothing is connected, then quits.
	alice := newNode(t, room, "alice", aliceDir, aliceID)
	if _, err := alice.app.AddContact(bob.id.Public().Address(), "bob"); err != nil {
		t.Fatal(err)
	}
	msg, err := alice.app.SendText(bob.id.Public(), "left the keys under the mat", "")
	if err != nil {
		t.Fatal(err)
	}
	alice.close()

	// Nothing reached bob.
	aliceChat := aliceID.NodeID().String()
	if len(bob.messages(aliceChat)) != 0 {
		t.Fatal("the message left despite nobody being connected")
	}

	// Alice starts again, same identity and same database, and this time bob
	// is in range.
	room2 := loopback.NewRoom()
	_ = room2
	alice2 := newNode(t, room, "alice2", aliceDir, aliceID)
	room.Connect("alice2", "bob")

	waitFor(t, "the queued message to arrive after a restart", func() bool {
		return len(bob.messages(aliceChat)) == 1
	})

	got := bob.messages(aliceChat)[0]
	if got.Body != "left the keys under the mat" {
		t.Fatalf("body is %q", got.Body)
	}
	// And the id is preserved, so a recipient who already had it would dedupe.
	if got.ID != msg.ID {
		t.Fatal("the resumed message was given a new id")
	}

	waitFor(t, "alice to see it delivered", func() bool {
		m, _ := alice2.store.Message(msg.ID)
		return m != nil && m.State == store.StateDelivered
	})
}

// A queued attachment has to be reloaded from the blob store on resume, not
// just its row.
func TestQueuedAttachmentResumesWithItsBytes(t *testing.T) {
	room := loopback.NewRoom()

	aliceDir := t.TempDir()
	aliceID, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	bob := newNode(t, room, "bob", t.TempDir(), nil)

	data := []byte("the quick brown fox jumps over the lazy dog")

	alice := newNode(t, room, "alice", aliceDir, aliceID)
	alice.app.AddContact(bob.id.Public().Address(), "bob")
	if _, err := alice.app.SendAttachment(bob.id.Public(), store.KindFile,
		"text/plain", "note.txt", data, ""); err != nil {
		t.Fatal(err)
	}
	alice.close()

	newNode(t, room, "alice2", aliceDir, aliceID)
	room.Connect("alice2", "bob")

	aliceChat := aliceID.NodeID().String()
	waitFor(t, "the queued attachment to arrive", func() bool {
		return len(bob.messages(aliceChat)) == 1
	})

	got := bob.messages(aliceChat)[0]
	if got.BlobID == "" {
		t.Fatal("the attachment did not survive the restart")
	}
	_, name, blob, _ := bob.store.Blob(got.BlobID)
	if name != "note.txt" || string(blob) != string(data) {
		t.Fatalf("attachment came back as %q %q", name, blob)
	}
}

// A queued message addressed to somebody no longer in the contact list cannot
// be sent and must not be reconsidered on every start for ever.
func TestUnaddressableQueuedMessageFails(t *testing.T) {
	room := loopback.NewRoom()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "yap.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.EnsureChat("ghost", "direct", "")
	st.AddMessage(&store.Message{
		ID: "orphan", ChatID: "ghost", Mine: true, Kind: store.KindText,
		Body: "into the void", CreatedAt: 1, ReceivedAt: 1, State: store.StateQueued,
	})
	st.Close()

	n := newNode(t, room, "solo", dir, nil)
	waitFor(t, "the orphan message to be marked failed", func() bool {
		m, _ := n.store.Message("orphan")
		return m != nil && m.State == store.StateFailed
	})
}

func TestBlockedContactIsIgnored(t *testing.T) {
	p := newPair(t)

	aliceChat := p.alice.id.NodeID().String()

	// Bob has to know alice before he can block her.
	if _, err := p.bob.app.AddContact(p.alice.id.Public().Address(), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := p.bob.store.SetBlocked(aliceChat, true); err != nil {
		t.Fatal(err)
	}

	if _, err := p.alice.app.SendText(p.bob.id.Public(), "let me in", ""); err != nil {
		t.Fatal(err)
	}

	// Give it long enough that an unblocked message would certainly have shown.
	time.Sleep(700 * time.Millisecond)
	if got := len(p.bob.messages(aliceChat)); got != 0 {
		t.Fatalf("a blocked contact delivered %d messages", got)
	}
}

func TestAddContactRejectsBadInput(t *testing.T) {
	p := newPair(t)

	if _, err := p.alice.app.AddContact("not an address", ""); err == nil {
		t.Fatal("a malformed address was accepted")
	}
	if _, err := p.alice.app.AddContact(p.alice.id.Public().Address(), ""); err == nil {
		t.Fatal("adding yourself was accepted")
	}
}

func TestSendToSelfRefused(t *testing.T) {
	p := newPair(t)
	if _, err := p.alice.app.SendText(p.alice.id.Public(), "hi me", ""); err == nil {
		t.Fatal("sending to yourself was accepted")
	}
}

// Deleting clears the content on both sides. It is a request the other node
// honours, not something the protocol can compel.
func TestDeleteForEveryone(t *testing.T) {
	p := newPair(t)

	msg, err := p.alice.app.SendText(p.bob.id.Public(), "regrettable", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceChat := p.alice.id.NodeID().String()
	waitFor(t, "bob to receive it", func() bool {
		return len(p.bob.messages(aliceChat)) == 1
	})

	if err := p.alice.app.DeleteForEveryone(msg.ID); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob's copy to be cleared", func() bool {
		m, _ := p.bob.store.Message(msg.ID)
		return m != nil && m.Deleted && m.Body == ""
	})

	mine, _ := p.alice.store.Message(msg.ID)
	if !mine.Deleted {
		t.Fatal("the sender's own copy was not cleared")
	}
}

// The UI stream is how anything appears without a reload.
func TestSubscribersSeeIncomingMessages(t *testing.T) {
	p := newPair(t)

	events, release := p.bob.app.Subscribe()
	defer release()

	if _, err := p.alice.app.SendText(p.bob.id.Public(), "ping", ""); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(4 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == "message" && ev.Message != nil && ev.Message.Body == "ping" {
				return
			}
		case <-deadline:
			t.Fatal("no message event reached a subscriber")
		}
	}
}

func TestReleasingASubscriptionIsSafe(t *testing.T) {
	p := newPair(t)

	_, release := p.alice.app.Subscribe()
	release()
	release() // must not panic on a double release

	// Publishing afterwards must not panic on the closed channel.
	p.alice.app.publish(Event{Type: "peers"})
}

func TestEnvelopeForMissingAttachment(t *testing.T) {
	p := newPair(t)

	// A row that references an attachment which is not in the blob store.
	env, err := p.alice.app.envelopeFor(&store.Message{
		ID: "m1", Kind: store.KindImage, Body: "caption", BlobID: "gone",
	})
	if err != nil {
		t.Fatalf("a missing attachment errored instead of degrading: %v", err)
	}
	if env.Data != "" {
		t.Fatal("data was invented for a missing attachment")
	}
	if env.Body != "caption" {
		t.Fatal("the caption was lost with the attachment")
	}
}

func TestEnvelopeForCarriesAttachmentBytes(t *testing.T) {
	p := newPair(t)

	raw := []byte("hello bytes")
	if err := p.alice.store.PutBlob("b1", "text/plain", "a.txt", raw); err != nil {
		t.Fatal(err)
	}

	env, err := p.alice.app.envelopeFor(&store.Message{
		ID: "m1", Kind: store.KindFile, BlobID: "b1",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("attachment encoded as %q", decoded)
	}
	if env.Mime != "text/plain" || env.Name != "a.txt" {
		t.Fatalf("metadata is %q %q", env.Mime, env.Name)
	}
}
