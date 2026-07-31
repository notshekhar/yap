package mesh

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/notshekhar/yap/internal/identity"
	"github.com/notshekhar/yap/internal/transport/loopback"
	"github.com/notshekhar/yap/internal/wire"
)

// A room of nodes wired however the test wants them.
type harness struct {
	t     *testing.T
	room  *loopback.Room
	nodes map[string]*Node
}

func newHarness(t *testing.T, names ...string) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		room:  loopback.NewRoom(),
		nodes: make(map[string]*Node),
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, name := range names {
		id, err := identity.New()
		if err != nil {
			t.Fatal(err)
		}
		n, err := New(Config{
			Identity:  id,
			Name:      name,
			Transport: h.room.Join(name),
			Logger:    quiet,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.Close() })
		h.nodes[name] = n
	}
	return h
}

func (h *harness) node(name string) *Node {
	n, ok := h.nodes[name]
	if !ok {
		h.t.Fatalf("no node named %q", name)
	}
	return n
}

func (h *harness) key(name string) identity.PublicKey { return h.node(name).id.Public() }

// waitForMessage blocks until a node receives something, or fails the test.
func waitForMessage(t *testing.T, n *Node, within time.Duration) Inbound {
	t.Helper()
	select {
	case in := <-n.Inbound():
		return in
	case <-time.After(within):
		t.Fatal("timed out waiting for a message")
		return Inbound{}
	}
}

func waitForDelivery(t *testing.T, n *Node, within time.Duration) Delivery {
	t.Helper()
	select {
	case d := <-n.Delivered():
		return d
	case <-time.After(within):
		t.Fatal("timed out waiting for a delivery report")
		return Delivery{}
	}
}

// eventually retries a condition until it holds or time runs out. Mesh work is
// asynchronous by nature, so polling beats sleeping a fixed amount.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestTwoNodesExchangeMessages(t *testing.T) {
	h := newHarness(t, "alice", "bob")
	h.room.Connect("alice", "bob")

	alice, bob := h.node("alice"), h.node("bob")

	if err := alice.Send(h.key("bob"), []byte("are you around?"), "m1"); err != nil {
		t.Fatal(err)
	}

	got := waitForMessage(t, bob, 2*time.Second)
	if string(got.Payload) != "are you around?" {
		t.Fatalf("bob got %q", got.Payload)
	}
	if got.From != h.key("alice") {
		t.Fatal("message was attributed to the wrong sender")
	}

	// And the sender learns it landed.
	d := waitForDelivery(t, alice, 2*time.Second)
	if !d.Acked || d.Ref != "m1" {
		t.Fatalf("delivery report was %+v", d)
	}

	// Now the other direction, which exercises the session bob built as
	// responder rather than initiator.
	if err := bob.Send(h.key("alice"), []byte("just got here"), "m2"); err != nil {
		t.Fatal(err)
	}
	back := waitForMessage(t, alice, 2*time.Second)
	if string(back.Payload) != "just got here" {
		t.Fatalf("alice got %q", back.Payload)
	}
}

// The product claim: a relay carries traffic it cannot read.
func TestRelayForwardsButCannotRead(t *testing.T) {
	h := newHarness(t, "alice", "carol", "bob")
	// A line, so alice and bob cannot hear each other at all.
	h.room.Chain("alice", "carol", "bob")

	alice, bob, carol := h.node("alice"), h.node("bob"), h.node("carol")

	if err := alice.Send(h.key("bob"), []byte("meet at six"), "m1"); err != nil {
		t.Fatal(err)
	}

	got := waitForMessage(t, bob, 3*time.Second)
	if string(got.Payload) != "meet at six" {
		t.Fatalf("bob got %q", got.Payload)
	}
	if got.Direct {
		t.Fatal("a relayed message was reported as direct")
	}

	// Carol moved the bytes and has no session with either end, so nothing
	// was ever decryptable by her.
	if _, ok := carol.sess.Session(alice.id.NodeID()); ok {
		t.Fatal("the relay somehow holds a session with the sender")
	}
	select {
	case in := <-carol.Inbound():
		t.Fatalf("the relay surfaced message content: %q", in.Payload)
	default:
	}
}

func TestFourHopChain(t *testing.T) {
	h := newHarness(t, "a", "b", "c", "d", "e")
	h.room.Chain("a", "b", "c", "d", "e")

	if err := h.node("a").Send(h.key("e"), []byte("all the way down"), "m1"); err != nil {
		t.Fatal(err)
	}
	got := waitForMessage(t, h.node("e"), 4*time.Second)
	if string(got.Payload) != "all the way down" {
		t.Fatalf("got %q", got.Payload)
	}
}

// The scenario that makes a mesh worth building: two people who are never in
// the same place, joined by someone who visits both.
func TestStoreAndForwardAcrossTime(t *testing.T) {
	h := newHarness(t, "alice", "courier", "bob")

	// Alice can reach the courier. Bob is not here yet.
	h.room.Connect("alice", "courier")
	alice, bob := h.node("alice"), h.node("bob")

	if err := alice.Send(h.key("bob"), []byte("left the keys under the mat"), "m1"); err != nil {
		t.Fatal(err)
	}

	// The courier should be holding it, undeliverable for now.
	eventually(t, 2*time.Second, "the courier to pick up the message", func() bool {
		return h.node("courier").Carrying() > 0
	})

	// Alice leaves; bob arrives and meets the courier.
	h.room.Disconnect("alice", "courier")
	h.room.Connect("courier", "bob")

	got := waitForMessage(t, bob, 4*time.Second)
	if string(got.Payload) != "left the keys under the mat" {
		t.Fatalf("bob got %q", got.Payload)
	}
}

// A message sent to nobody must survive until its recipient turns up.
func TestMessageWaitsForAnAbsentPeer(t *testing.T) {
	h := newHarness(t, "alice", "bob")
	alice, bob := h.node("alice"), h.node("bob")

	// Nobody is connected to anybody.
	if err := alice.Send(h.key("bob"), []byte("when you get in"), "m1"); err != nil {
		t.Fatal(err)
	}
	if alice.Pending() != 1 {
		t.Fatalf("expected 1 pending message, got %d", alice.Pending())
	}

	h.room.Connect("alice", "bob")

	got := waitForMessage(t, bob, 4*time.Second)
	if string(got.Payload) != "when you get in" {
		t.Fatalf("bob got %q", got.Payload)
	}
	eventually(t, 3*time.Second, "the outbox to drain", func() bool {
		return alice.Pending() == 0
	})
}

// Dedupe is the difference between a flood and a broadcast storm. In a fully
// connected room, one message must not multiply.
func TestFloodDoesNotStorm(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f"}
	h := newHarness(t, names...)
	h.room.Mesh(names...)

	if err := h.node("a").Send(h.key("f"), []byte("hello everyone"), "m1"); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, h.node("f"), 3*time.Second)

	// Let any storm that is going to happen, happen.
	time.Sleep(300 * time.Millisecond)

	// Every node's dedupe set should hold a small, bounded number of ids: the
	// announces plus a handful of data and handshake packets. A storm would
	// show up here as a number in the thousands.
	for _, name := range names {
		if got := h.node(name).seen.Len(); got > 100 {
			t.Fatalf("node %s saw %d distinct packets, which suggests a storm", name, got)
		}
	}
}

func TestPeersAreDiscovered(t *testing.T) {
	names := []string{"a", "b", "c"}
	h := newHarness(t, names...)
	h.room.Mesh(names...)

	eventually(t, 3*time.Second, "every node to see the other two", func() bool {
		for _, name := range names {
			if len(h.node(name).Peers()) != 2 {
				return false
			}
		}
		return true
	})

	// A discovered peer carries a usable key and its chosen name.
	peers := h.node("a").Peers()
	for _, p := range peers {
		if p.Name != "b" && p.Name != "c" {
			t.Fatalf("unexpected peer name %q", p.Name)
		}
		if p.Key.NodeID() != p.NodeID() {
			t.Fatal("peer key and node id disagree")
		}
	}
}

// An announce is unauthenticated, so a hostile node can claim any name. It
// must not be able to claim someone else's *identity*: the key has to hash to
// the source it arrived from.
func TestForgedAnnounceIsRejected(t *testing.T) {
	h := newHarness(t, "alice", "mallory")
	h.room.Connect("alice", "mallory")

	victim, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}

	// Mallory announces the victim's key from her own node id. sendPacket
	// stamps the real source, which is exactly the mismatch the receiver
	// checks for.
	h.node("mallory").sendPacket(&wire.Packet{
		Type:    wire.TypeAnnounce,
		Payload: encodeAnnounce(victim.Public(), "the victim"),
	})

	time.Sleep(200 * time.Millisecond)

	for _, p := range h.node("alice").Peers() {
		if p.Key == victim.Public() {
			t.Fatal("a forged announce planted someone else's identity")
		}
	}
}

func TestMessagesSurviveLossyLinks(t *testing.T) {
	h := newHarness(t, "alice", "bob")
	h.room.Connect("alice", "bob")
	// Bluetooth in a crowded room genuinely loses this much.
	h.room.SetLoss(0.3)

	alice, bob := h.node("alice"), h.node("bob")
	if err := alice.Send(h.key("bob"), []byte("through the noise"), "m1"); err != nil {
		t.Fatal(err)
	}

	// Retries are what carry it through, so allow more than one interval.
	got := waitForMessage(t, bob, 3*time.Second)
	if string(got.Payload) != "through the noise" {
		t.Fatalf("bob got %q", got.Payload)
	}
}

func TestSendToSelfIsRefused(t *testing.T) {
	h := newHarness(t, "alice")
	alice := h.node("alice")
	if err := alice.Send(h.key("alice"), []byte("hi me"), "m1"); err == nil {
		t.Fatal("sending to yourself was allowed")
	}
}

func TestManyMessagesArriveIntact(t *testing.T) {
	h := newHarness(t, "alice", "bob")
	h.room.Connect("alice", "bob")
	alice, bob := h.node("alice"), h.node("bob")

	const count = 25
	for i := 0; i < count; i++ {
		payload := []byte{byte(i)}
		if err := alice.Send(h.key("bob"), payload, "m"); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[byte]bool)
	deadline := time.After(6 * time.Second)
	for len(seen) < count {
		select {
		case in := <-bob.Inbound():
			if len(in.Payload) == 1 {
				seen[in.Payload[0]] = true
			}
		case <-deadline:
			t.Fatalf("only %d of %d messages arrived", len(seen), count)
		}
	}
}

// A message larger than one BLE frame has to survive fragmentation. The
// loopback transport does not fragment, so this checks the mesh's own limits
// rather than the radio's.
func TestLargePayload(t *testing.T) {
	h := newHarness(t, "alice", "bob")
	h.room.Connect("alice", "bob")

	payload := make([]byte, 20000)
	for i := range payload {
		payload[i] = byte(i)
	}

	if err := h.node("alice").Send(h.key("bob"), payload, "big"); err != nil {
		t.Fatal(err)
	}
	got := waitForMessage(t, h.node("bob"), 5*time.Second)
	if len(got.Payload) != len(payload) {
		t.Fatalf("got %d bytes, want %d", len(got.Payload), len(payload))
	}
	for i := range payload {
		if got.Payload[i] != payload[i] {
			t.Fatalf("payload differs at byte %d", i)
		}
	}
}
