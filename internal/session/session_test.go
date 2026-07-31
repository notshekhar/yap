package session

import (
	"bytes"
	"errors"
	"testing"

	"github.com/notshekhar/yap/internal/identity"
)

type node struct {
	id  *identity.Identity
	mgr *Manager
}

func newNode(t *testing.T) *node {
	t.Helper()
	id, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	return &node{id: id, mgr: NewManager(id)}
}

// handshake runs a full exchange and leaves both sides established.
func handshake(t *testing.T, a, b *node, firstPayload []byte) *Result {
	t.Helper()

	init, err := a.mgr.Initiate(b.id.Public(), firstPayload)
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	got, err := b.mgr.HandleHandshake(init)
	if err != nil {
		t.Fatalf("responder read init: %v", err)
	}
	if !got.Established {
		t.Fatal("responder did not establish")
	}
	if got.Reply == nil {
		t.Fatal("responder produced no reply")
	}

	done, err := a.mgr.HandleHandshake(got.Reply)
	if err != nil {
		t.Fatalf("initiator read response: %v", err)
	}
	if !done.Established {
		t.Fatal("initiator did not establish")
	}
	return got
}

func TestHandshakeEstablishesBothDirections(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	// Each side must have authenticated the other's real static key, not
	// merely agreed on something.
	sa, ok := a.mgr.Session(b.id.NodeID())
	if !ok {
		t.Fatal("initiator has no session")
	}
	if sa.Peer() != b.id.Public() {
		t.Fatal("initiator authenticated the wrong key")
	}
	sb, ok := b.mgr.Session(a.id.NodeID())
	if !ok {
		t.Fatal("responder has no session")
	}
	if sb.Peer() != a.id.Public() {
		t.Fatal("responder authenticated the wrong key")
	}

	for _, dir := range []struct {
		name     string
		from, to *node
	}{
		{"a to b", a, b},
		{"b to a", b, a},
	} {
		t.Run(dir.name, func(t *testing.T) {
			sealed, err := dir.from.mgr.Seal(dir.to.id.NodeID(), []byte("hello there"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := dir.to.mgr.Open(dir.from.id.NodeID(), sealed)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != "hello there" {
				t.Fatalf("got %q", out)
			}
		})
	}
}

// The whole point of choosing IK: the first message carries content.
func TestZeroRTTPayloadArrivesWithHandshake(t *testing.T) {
	a, b := newNode(t), newNode(t)
	res := handshake(t, a, b, []byte("meet me at the usual place"))

	if string(res.Payload) != "meet me at the usual place" {
		t.Fatalf("0-RTT payload was %q", res.Payload)
	}
}

// A relay forwards ciphertext it must not be able to read. This is the claim
// the product makes; it deserves an explicit test rather than an assumption.
func TestRelayCannotDecrypt(t *testing.T) {
	a, b, relay := newNode(t), newNode(t), newNode(t)
	handshake(t, a, b, nil)

	sealed, err := a.mgr.Seal(b.id.NodeID(), []byte("private"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := relay.mgr.Open(a.id.NodeID(), sealed); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a relay could act on the ciphertext: %v", err)
	}
	if bytes.Contains(sealed, []byte("private")) {
		t.Fatal("plaintext is visible in the sealed message")
	}
}

// Someone who holds a different key must not be able to answer for an address
// they do not own.
func TestImpostorCannotAnswerForAnAddress(t *testing.T) {
	a, real, impostor := newNode(t), newNode(t), newNode(t)

	// a addresses the real peer, but the impostor intercepts the message.
	init, err := a.mgr.Initiate(real.id.Public(), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := impostor.mgr.HandleHandshake(init); err == nil {
		t.Fatal("an impostor decrypted a handshake addressed to someone else")
	}
}

// The property that makes this usable on Bluetooth at all. With Noise's
// implicit counters, dropping message 2 would desynchronise the pair and every
// later message would fail.
func TestSurvivesLossAndReordering(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	var sealed [][]byte
	for i := 0; i < 10; i++ {
		s, err := a.mgr.Seal(b.id.NodeID(), []byte{byte('0' + i)})
		if err != nil {
			t.Fatal(err)
		}
		sealed = append(sealed, s)
	}

	// Deliver in a deliberately awful order, with two messages lost entirely.
	order := []int{3, 1, 0, 9, 4, 2, 8, 5} // 6 and 7 never arrive
	var got []byte
	for _, i := range order {
		out, err := b.mgr.Open(a.id.NodeID(), sealed[i])
		if err != nil {
			t.Fatalf("message %d failed after reordering: %v", i, err)
		}
		got = append(got, out...)
	}

	if want := "31094285"; string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// And the pair is still healthy afterwards.
	s, err := a.mgr.Seal(b.id.NodeID(), []byte("still working"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.mgr.Open(a.id.NodeID(), s)
	if err != nil {
		t.Fatalf("session broke after loss: %v", err)
	}
	if string(out) != "still working" {
		t.Fatalf("got %q", out)
	}
}

// Explicit nonces buy loss tolerance and owe replay protection in return.
func TestReplayRejected(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	sealed, err := a.mgr.Seal(b.id.NodeID(), []byte("transfer the money"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b.mgr.Open(a.id.NodeID(), sealed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := b.mgr.Open(a.id.NodeID(), sealed); !errors.Is(err, ErrReplay) {
			t.Fatalf("replay %d was accepted: %v", i, err)
		}
	}
}

// A message far behind the window cannot be proven fresh, so it is refused
// rather than gambled on.
func TestAncientMessageRefused(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	old, err := a.mgr.Seal(b.id.NodeID(), []byte("very old"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < windowSize+10; i++ {
		s, err := a.mgr.Seal(b.id.NodeID(), []byte("filler"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.mgr.Open(a.id.NodeID(), s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.mgr.Open(a.id.NodeID(), old); !errors.Is(err, ErrReplay) {
		t.Fatalf("a message older than the window was accepted: %v", err)
	}
}

func TestTamperingDetected(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	sealed, err := a.mgr.Seal(b.id.NodeID(), []byte("pay alice 10"))
	if err != nil {
		t.Fatal(err)
	}

	for i := nonceSize; i < len(sealed); i++ {
		bad := bytes.Clone(sealed)
		bad[i] ^= 0x01
		if _, err := b.mgr.Open(a.id.NodeID(), bad); err == nil {
			t.Fatalf("flipping a bit at offset %d went undetected", i)
		}
	}
}

// A forged message must not be able to consume the replay slot belonging to a
// genuine one that has not arrived yet.
func TestForgeryDoesNotBurnReplaySlot(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	genuine, err := a.mgr.Seal(b.id.NodeID(), []byte("the real message"))
	if err != nil {
		t.Fatal(err)
	}

	forged := bytes.Clone(genuine)
	forged[len(forged)-1] ^= 0xff
	if _, err := b.mgr.Open(a.id.NodeID(), forged); err == nil {
		t.Fatal("forged message was accepted")
	}

	out, err := b.mgr.Open(a.id.NodeID(), genuine)
	if err != nil {
		t.Fatalf("genuine message was locked out by a forgery: %v", err)
	}
	if string(out) != "the real message" {
		t.Fatalf("got %q", out)
	}
}

func TestSealWithoutSessionFails(t *testing.T) {
	a, b := newNode(t), newNode(t)
	if _, err := a.mgr.Seal(b.id.NodeID(), []byte("hi")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestForgetDropsSession(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	a.mgr.Forget(b.id.NodeID())
	if _, ok := a.mgr.Session(b.id.NodeID()); ok {
		t.Fatal("session survived Forget")
	}
	if _, err := a.mgr.Seal(b.id.NodeID(), []byte("hi")); !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

// A node that restarts loses its sessions. The peer must be able to re-handshake
// rather than being stuck with a dead one.
func TestRehandshakeAfterRestart(t *testing.T) {
	a, b := newNode(t), newNode(t)
	handshake(t, a, b, nil)

	// b restarts: same identity, no sessions.
	b.mgr = NewManager(b.id)

	handshake(t, a, b, []byte("still here?"))

	sealed, err := b.mgr.Seal(a.id.NodeID(), []byte("back online"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.mgr.Open(b.id.NodeID(), sealed)
	if err != nil {
		t.Fatalf("could not resume after restart: %v", err)
	}
	if string(out) != "back online" {
		t.Fatalf("got %q", out)
	}
}

func TestConcurrentHandshakesToDifferentPeers(t *testing.T) {
	a := newNode(t)
	peers := []*node{newNode(t), newNode(t), newNode(t)}

	// Initiate to all three before answering any, so the response matcher has
	// to pick the right outstanding handshake out of several.
	var inits [][]byte
	for _, p := range peers {
		msg, err := a.mgr.Initiate(p.id.Public(), nil)
		if err != nil {
			t.Fatal(err)
		}
		inits = append(inits, msg)
	}

	for i, p := range peers {
		res, err := p.mgr.HandleHandshake(inits[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.mgr.HandleHandshake(res.Reply); err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
	}

	for _, p := range peers {
		sealed, err := a.mgr.Seal(p.id.NodeID(), []byte("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.mgr.Open(a.id.NodeID(), sealed); err != nil {
			t.Fatalf("session with %s is wrong: %v", p.id.NodeID(), err)
		}
	}
}

func TestReplayWindow(t *testing.T) {
	var w window

	if !w.check(5) {
		t.Fatal("first nonce refused")
	}
	w.accept(5)

	if w.check(5) {
		t.Fatal("accepted nonce was not recorded")
	}
	if !w.check(4) {
		t.Fatal("older-but-unseen nonce refused")
	}
	w.accept(4)
	if w.check(4) {
		t.Fatal("nonce 4 accepted twice")
	}

	if !w.check(200) {
		t.Fatal("far-ahead nonce refused")
	}
	w.accept(200)
	if w.check(5) {
		t.Fatal("nonce far behind the window was accepted")
	}
	if !w.check(199) {
		t.Fatal("nonce just inside the window refused")
	}
}
