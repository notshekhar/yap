// Package session provides the end-to-end encryption between two yap users.
//
// The pattern is Noise IK over X25519 / ChaChaPoly / SHA256. IK is chosen over
// the more familiar XX because of what a yap address contains: the peer's full
// static public key. Knowing it up front means the very first message can be
// encrypted and authenticated, with no round trip to negotiate first. On a
// mesh where the recipient may be two relays away and asleep, a pattern that
// needs three messages before any content can move is close to useless.
//
// # Nonces are explicit, and that is not optional
//
// Noise's transport phase assumes an in-order, reliable channel: each side
// keeps a counter and increments it per message. A Bluetooth mesh is neither.
// Frames are dropped when someone walks behind a wall, and arrive out of order
// when two relays race. With an implicit counter, one lost message
// desynchronises the pair permanently and every subsequent message fails to
// decrypt — the kind of bug that looks like "chat randomly stops working".
//
// So every sealed message carries its nonce, and the receiver sets it before
// decrypting. That restores tolerance to loss and reordering, and hands back
// the problem Noise's counter was quietly solving: replay. A sliding window of
// recently seen nonces closes it, the same trick IPsec and DTLS use.
package session

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/flynn/noise"

	"github.com/notshekhar/yap/internal/identity"
)

// prologue binds every handshake to this protocol and version. A peer running
// a different version fails the handshake outright instead of half-working.
var prologue = []byte("yap-noise-ik-v1")

var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// Handshake message kinds, carried as a one-byte tag so a receiver knows
// whether it is being greeted or answered.
const (
	tagInit byte = 1
	tagResp byte = 2
)

// nonceSize is the explicit nonce prefixed to every sealed message.
const nonceSize = 8

// Errors callers are expected to branch on.
var (
	// ErrNoSession means nothing is established with this peer yet. The caller
	// should initiate a handshake.
	ErrNoSession = errors.New("session: no session with peer")

	// ErrReplay means a message with an already-seen nonce arrived. It is
	// either a duplicate from the mesh flood, which is normal and harmless, or
	// an attacker replaying traffic, which is not.
	ErrReplay = errors.New("session: message replayed")

	// ErrPeerMismatch means the static key presented in a handshake was not
	// the key we addressed. Someone is answering for an address they do not
	// hold.
	ErrPeerMismatch = errors.New("session: peer presented an unexpected static key")
)

// Session is an established, forward-secret channel with one peer.
type Session struct {
	mu   sync.Mutex
	peer identity.PublicKey
	send *noise.CipherState
	recv *noise.CipherState

	// sendNonce is ours to choose; the peer follows it rather than tracking
	// its own count.
	sendNonce uint64

	// replay guards the receive direction.
	replay window
}

// Peer returns the static public key this session authenticated.
func (s *Session) Peer() identity.PublicKey { return s.peer }

// Seal encrypts a payload for the peer. The returned bytes carry the nonce in
// the clear, followed by the ciphertext; both are safe for a relay to see.
func (s *Session) Seal(plaintext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := s.sendNonce
	s.sendNonce++

	s.send.SetNonce(n)
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+16)
	binary.BigEndian.PutUint64(out, n)

	sealed, err := s.send.Encrypt(out, nil, plaintext)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return sealed, nil
}

// Open decrypts a payload from the peer.
func (s *Session) Open(sealed []byte) ([]byte, error) {
	if len(sealed) < nonceSize {
		return nil, errors.New("session: sealed message is too short to hold a nonce")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	n := binary.BigEndian.Uint64(sealed[:nonceSize])
	if !s.replay.check(n) {
		return nil, ErrReplay
	}

	s.recv.SetNonce(n)
	out, err := s.recv.Decrypt(nil, nil, sealed[nonceSize:])
	if err != nil {
		// Do not record the nonce: a forged message must not be able to burn
		// a slot in the replay window and lock out the genuine one.
		return nil, fmt.Errorf("open: %w", err)
	}

	s.replay.accept(n)
	return out, nil
}

// pendingHandshake is an initiated handshake awaiting its answer.
type pendingHandshake struct {
	state *noise.HandshakeState
	peer  identity.PublicKey
}

// Manager holds this node's identity and its sessions with every peer.
type Manager struct {
	mu       sync.Mutex
	id       *identity.Identity
	static   noise.DHKey
	sessions map[identity.NodeID]*Session
	pending  map[identity.NodeID]*pendingHandshake
}

// NewManager builds a session manager for the given identity.
func NewManager(id *identity.Identity) *Manager {
	pub := id.Public()
	return &Manager{
		id: id,
		static: noise.DHKey{
			Private: id.PrivateBytes(),
			Public:  pub[:],
		},
		sessions: make(map[identity.NodeID]*Session),
		pending:  make(map[identity.NodeID]*pendingHandshake),
	}
}

// Session returns the established session with a peer, if there is one.
func (m *Manager) Session(peer identity.NodeID) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[peer]
	return s, ok
}

// Initiate starts a handshake with a peer whose address we know, optionally
// carrying a first payload. The payload rides inside the handshake message and
// is encrypted, which is the entire reason for choosing IK.
//
// The 0-RTT payload has a caveat worth stating plainly: unlike later traffic
// it has no forward secrecy against a future compromise of the recipient's
// static key, and a network attacker can replay the whole handshake message.
// Application-level message ids make a replayed delivery a duplicate rather
// than a new event, which is what the mesh dedupe already assumes.
func (m *Manager) Initiate(peer identity.PublicKey, payload []byte) ([]byte, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     true,
		Prologue:      prologue,
		StaticKeypair: m.static,
		PeerStatic:    peer[:],
	})
	if err != nil {
		return nil, fmt.Errorf("start handshake: %w", err)
	}

	msg, _, _, err := hs.WriteMessage([]byte{tagInit}, payload)
	if err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	m.mu.Lock()
	m.pending[peer.NodeID()] = &pendingHandshake{state: hs, peer: peer}
	m.mu.Unlock()

	return msg, nil
}

// Result reports what a received handshake message produced.
type Result struct {
	// Peer is the authenticated static key of the other side.
	Peer identity.PublicKey

	// Payload is any 0-RTT content that rode along, possibly empty.
	Payload []byte

	// Reply must be sent back when non-nil. It completes the handshake.
	Reply []byte

	// Established is true once a session is usable in both directions.
	Established bool
}

// HandleHandshake processes an incoming handshake message from either role.
func (m *Manager) HandleHandshake(msg []byte) (*Result, error) {
	if len(msg) < 1 {
		return nil, errors.New("session: empty handshake message")
	}
	switch msg[0] {
	case tagInit:
		return m.handleInit(msg)
	case tagResp:
		return m.handleResp(msg)
	default:
		return nil, fmt.Errorf("session: unknown handshake tag %d", msg[0])
	}
}

// handleInit answers a peer who greeted us. We had no prior knowledge of them;
// IK reveals their static key to us inside the encrypted portion of message
// one, which is what makes the sender authenticated rather than anonymous.
func (m *Manager) handleInit(msg []byte) (*Result, error) {
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cipherSuite,
		Random:        rand.Reader,
		Pattern:       noise.HandshakeIK,
		Initiator:     false,
		Prologue:      prologue,
		StaticKeypair: m.static,
	})
	if err != nil {
		return nil, fmt.Errorf("accept handshake: %w", err)
	}

	payload, _, _, err := hs.ReadMessage(nil, msg[1:])
	if err != nil {
		return nil, fmt.Errorf("read handshake init: %w", err)
	}

	var peer identity.PublicKey
	raw := hs.PeerStatic()
	if len(raw) != identity.KeySize {
		return nil, fmt.Errorf("session: peer static key is %d bytes", len(raw))
	}
	copy(peer[:], raw)

	reply, cs0, cs1, err := hs.WriteMessage([]byte{tagResp}, nil)
	if err != nil {
		return nil, fmt.Errorf("write handshake response: %w", err)
	}
	if cs0 == nil || cs1 == nil {
		return nil, errors.New("session: handshake did not complete on the responder")
	}

	// The responder sends on cs1 and receives on cs0; the initiator is the
	// mirror. Getting this backwards produces a session that encrypts fine and
	// never decrypts, so it is worth naming rather than indexing.
	sess := &Session{peer: peer, send: cs1, recv: cs0}

	m.mu.Lock()
	m.sessions[peer.NodeID()] = sess
	delete(m.pending, peer.NodeID())
	m.mu.Unlock()

	return &Result{Peer: peer, Payload: payload, Reply: reply, Established: true}, nil
}

// handleResp completes a handshake we started.
func (m *Manager) handleResp(msg []byte) (*Result, error) {
	// The response does not name its sender, so match it against outstanding
	// handshakes. In practice there is at most a handful.
	m.mu.Lock()
	candidates := make([]*pendingHandshake, 0, len(m.pending))
	for _, p := range m.pending {
		candidates = append(candidates, p)
	}
	m.mu.Unlock()

	if len(candidates) == 0 {
		return nil, errors.New("session: handshake response with no handshake outstanding")
	}

	for _, p := range candidates {
		payload, cs0, cs1, err := p.state.ReadMessage(nil, msg[1:])
		if err != nil {
			continue // not the handshake this answers
		}
		if cs0 == nil || cs1 == nil {
			return nil, errors.New("session: handshake did not complete on the initiator")
		}

		sess := &Session{peer: p.peer, send: cs0, recv: cs1}

		m.mu.Lock()
		m.sessions[p.peer.NodeID()] = sess
		delete(m.pending, p.peer.NodeID())
		m.mu.Unlock()

		return &Result{Peer: p.peer, Payload: payload, Established: true}, nil
	}
	return nil, errors.New("session: handshake response did not match any outstanding handshake")
}

// Seal encrypts for an established peer.
func (m *Manager) Seal(peer identity.NodeID, plaintext []byte) ([]byte, error) {
	s, ok := m.Session(peer)
	if !ok {
		return nil, ErrNoSession
	}
	return s.Seal(plaintext)
}

// Open decrypts from an established peer.
func (m *Manager) Open(peer identity.NodeID, sealed []byte) ([]byte, error) {
	s, ok := m.Session(peer)
	if !ok {
		return nil, ErrNoSession
	}
	return s.Open(sealed)
}

// Forget drops a session, forcing a fresh handshake next time. Used when a
// peer restarts and its old keys are gone.
func (m *Manager) Forget(peer identity.NodeID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, peer)
	delete(m.pending, peer)
}

// Peers lists the peers with live sessions.
func (m *Manager) Peers() []identity.NodeID {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]identity.NodeID, 0, len(m.sessions))
	for id := range m.sessions {
		out = append(out, id)
	}
	return out
}

// window is a sliding replay filter over explicit nonces.
//
// It accepts any nonce ahead of the highest seen, and any nonce within 64 of
// it that has not been seen before. Anything older is refused: on a mesh that
// reorders by a frame or two, 64 is generous, and the alternative — an
// unbounded set of every nonce ever seen — is a memory leak.
type window struct {
	highest uint64
	bits    uint64
	started bool
}

const windowSize = 64

// check reports whether a nonce is acceptable, without recording it.
func (w *window) check(n uint64) bool {
	if !w.started {
		return true
	}
	if n > w.highest {
		return true
	}
	if w.highest-n >= windowSize {
		return false // too old to prove it is not a replay
	}
	return w.bits&(1<<(w.highest-n)) == 0
}

// accept records a nonce that decrypted successfully.
func (w *window) accept(n uint64) {
	if !w.started {
		w.started = true
		w.highest = n
		w.bits = 1
		return
	}
	if n > w.highest {
		shift := n - w.highest
		if shift >= windowSize {
			w.bits = 0
		} else {
			w.bits <<= shift
		}
		w.bits |= 1
		w.highest = n
		return
	}
	if w.highest-n < windowSize {
		w.bits |= 1 << (w.highest - n)
	}
}
