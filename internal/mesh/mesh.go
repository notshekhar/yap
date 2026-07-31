// Package mesh is the router: it decides what to do with every packet that
// arrives and how to get one to a peer who may be several rooms away.
//
// The routing model is a controlled flood. There is no routing protocol, no
// link-state exchange and no attempt to compute paths, because the topology
// here changes every time somebody stands up. A packet is broadcast to every
// neighbour, each neighbour rebroadcasts it once, and a time-to-live stops it
// eventually. Two things make that affordable: a dedupe set, so each node
// relays a given packet exactly once, and a shortcut for peers we can already
// hear directly, so the common case of two people in the same room does not
// involve the rest of the building.
//
// Everything a relay handles is opaque. It reads a destination and a hop
// count; the payload is a sealed Noise ciphertext it has no key for.
package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/notshekhar/yap/internal/identity"
	"github.com/notshekhar/yap/internal/session"
	"github.com/notshekhar/yap/internal/transport"
	"github.com/notshekhar/yap/internal/wire"
)

// Tunables. These are the knobs that decide whether a crowded room works or
// melts down, so they are named and gathered rather than sprinkled inline.
const (
	// AnnounceInterval is how often a node says it is here. Frequent enough
	// that a newcomer is noticed quickly, rare enough that fifty people in a
	// room do not saturate the air with presence traffic.
	AnnounceInterval = 20 * time.Second

	// PeerExpiry is how long a peer stays listed after its last announce.
	PeerExpiry = 90 * time.Second

	// RetryInterval is how often undelivered messages are tried again.
	RetryInterval = 15 * time.Second

	// MaxRetries bounds redelivery before a message is marked failed.
	MaxRetries = 40

	// OutboxLimit bounds messages awaiting delivery, so a node that spends a
	// week out of range does not grow without limit.
	OutboxLimit = 2048

	// CacheLimit bounds packets a relay holds on behalf of *other* people.
	// This is the store-and-forward pool, and it is a service to the mesh
	// rather than to us, so it gets a modest share.
	CacheLimit = 512

	// CacheTTL is how long a relay carries someone else's undelivered packet.
	CacheTTL = 10 * time.Minute

	// seenLimit and seenTTL size the dedupe set.
	seenLimit = 8192
	seenTTL   = 10 * time.Minute
)

// Peer is what we know about another node.
type Peer struct {
	Key      identity.PublicKey
	Name     string
	LastSeen time.Time

	// Direct is true when we can hear this peer without a relay.
	Direct bool

	// Hops is the smallest relay count seen from this peer, 0 when direct.
	Hops int
}

// NodeID returns the peer's routing handle.
func (p Peer) NodeID() identity.NodeID { return p.Key.NodeID() }

// Inbound is a decrypted application payload from an authenticated peer.
type Inbound struct {
	From    identity.PublicKey
	Payload []byte

	// Direct reports whether it arrived without relaying.
	Direct bool
}

// Delivery reports the fate of something we sent.
type Delivery struct {
	// Ref is the caller's own identifier, echoed back so it can match this to
	// whatever it is tracking.
	Ref string

	// Acked is true when the recipient confirmed receipt.
	Acked bool

	// Err is set when the message was abandoned.
	Err error
}

// outgoing is a message awaiting acknowledgement.
type outgoing struct {
	ref      string
	to       identity.PublicKey
	payload  []byte
	msgID    wire.MsgID
	attempts int
	next     time.Time
	// handshakeSent records that we have already tried to open a session, so
	// repeated retries do not spray handshakes.
	handshakeSent bool
}

// cached is a packet a relay is carrying for someone not currently reachable.
type cached struct {
	packet   *wire.Packet
	deadline time.Time
}

// Node is one participant in the mesh.
type Node struct {
	id   *identity.Identity
	name string
	tr   transport.Transport
	sess *session.Manager
	log  *slog.Logger
	seen *wire.Seen

	mu     sync.Mutex
	links  map[transport.LinkID]int             // link -> mtu
	routes map[identity.NodeID]transport.LinkID // peer -> link it was last heard on
	peers  map[identity.NodeID]*Peer
	outbox map[wire.MsgID]*outgoing
	cache  []cached

	inbound   chan Inbound
	delivered chan Delivery

	now  func() time.Time
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// Config configures a node.
type Config struct {
	Identity  *identity.Identity
	Name      string
	Transport transport.Transport
	Logger    *slog.Logger
}

// New builds a mesh node.
func New(cfg Config) (*Node, error) {
	if cfg.Identity == nil {
		return nil, errors.New("mesh: identity is required")
	}
	if cfg.Transport == nil {
		return nil, errors.New("mesh: transport is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Node{
		id:        cfg.Identity,
		name:      cfg.Name,
		tr:        cfg.Transport,
		sess:      session.NewManager(cfg.Identity),
		log:       log,
		seen:      wire.NewSeen(seenLimit, seenTTL),
		links:     make(map[transport.LinkID]int),
		routes:    make(map[identity.NodeID]transport.LinkID),
		peers:     make(map[identity.NodeID]*Peer),
		outbox:    make(map[wire.MsgID]*outgoing),
		inbound:   make(chan Inbound, 256),
		delivered: make(chan Delivery, 256),
		now:       time.Now,
		stop:      make(chan struct{}),
	}, nil
}

// Identity returns this node's identity.
func (n *Node) Identity() *identity.Identity { return n.id }

// Address returns this node's printable address.
func (n *Node) Address() string { return n.id.Address() }

// Inbound is the stream of decrypted messages addressed to us.
func (n *Node) Inbound() <-chan Inbound { return n.inbound }

// Delivered is the stream of delivery outcomes for messages we sent.
func (n *Node) Delivered() <-chan Delivery { return n.delivered }

// Start brings the node up and begins processing.
func (n *Node) Start(ctx context.Context) error {
	if err := n.tr.Start(ctx); err != nil {
		return fmt.Errorf("start transport: %w", err)
	}

	n.wg.Add(2)
	go n.readLoop()
	go n.tickLoop()

	// Announce immediately so a node that just launched is visible without
	// waiting out a full interval.
	n.announce()
	return nil
}

// Close shuts the node down.
func (n *Node) Close() error {
	n.once.Do(func() {
		close(n.stop)
		n.tr.Close()
		n.wg.Wait()
		close(n.inbound)
		close(n.delivered)
	})
	return nil
}

// Send delivers a payload to a peer, encrypted end to end. It returns as soon
// as the message is queued; the outcome arrives on Delivered.
//
// ref is echoed back in the Delivery so the caller can correlate without
// having to understand message ids.
func (n *Node) Send(to identity.PublicKey, payload []byte, ref string) error {
	if to == n.id.Public() {
		return errors.New("mesh: cannot send to yourself")
	}

	msgID, err := wire.NewMsgID()
	if err != nil {
		return err
	}

	n.mu.Lock()
	if len(n.outbox) >= OutboxLimit {
		n.mu.Unlock()
		return fmt.Errorf("mesh: outbox is full (%d messages undelivered)", OutboxLimit)
	}
	out := &outgoing{
		ref:     ref,
		to:      to,
		payload: append([]byte(nil), payload...),
		msgID:   msgID,
		next:    n.now(),
	}
	n.outbox[msgID] = out
	n.mu.Unlock()

	n.attempt(out)
	return nil
}

// Peers lists everyone currently known, freshest first.
func (n *Node) Peers() []Peer {
	n.mu.Lock()
	defer n.mu.Unlock()

	cutoff := n.now().Add(-PeerExpiry)
	out := make([]Peer, 0, len(n.peers))
	for _, p := range n.peers {
		if p.LastSeen.After(cutoff) {
			out = append(out, *p)
		}
	}
	return out
}

// LinkCount reports how many neighbours are currently connected.
func (n *Node) LinkCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.links)
}

// Pending reports how many of our own messages are awaiting acknowledgement.
func (n *Node) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.outbox)
}

// Carrying reports how many packets we are holding for other people.
func (n *Node) Carrying() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.cache)
}

// readLoop consumes transport events until the transport closes.
func (n *Node) readLoop() {
	defer n.wg.Done()
	events := n.tr.Events()
	for {
		select {
		case <-n.stop:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			n.handleEvent(ev)
		}
	}
}

func (n *Node) handleEvent(ev transport.Event) {
	switch ev.Kind {
	case transport.LinkUp:
		n.mu.Lock()
		n.links[ev.Link] = ev.MTU
		n.mu.Unlock()

		// A new neighbour is the only chance a stalled message has, so greet
		// them and immediately try everything that is stuck.
		n.announce()
		n.flushCache()
		n.retryOutbox(true)

	case transport.LinkDown:
		n.mu.Lock()
		delete(n.links, ev.Link)
		for peer, link := range n.routes {
			if link == ev.Link {
				delete(n.routes, peer)
				if p := n.peers[peer]; p != nil {
					p.Direct = false
				}
			}
		}
		n.mu.Unlock()

	case transport.PacketReceived:
		n.handlePacket(ev.Link, ev.Packet)
	}
}

func (n *Node) handlePacket(link transport.LinkID, raw []byte) {
	pkt, err := wire.Decode(raw)
	if err != nil {
		n.log.Debug("undecodable packet", "err", err, "link", link)
		return
	}

	// Our own packet returning to us on a loop: drop it silently.
	if pkt.Src == n.id.NodeID() {
		return
	}

	// Dedupe before anything else. This is what stops the flood squaring.
	if !n.seen.Mark(pkt.ID) {
		return
	}

	// Remember where this peer was last heard, for the unicast shortcut.
	n.mu.Lock()
	n.routes[pkt.Src] = link
	n.mu.Unlock()

	forUs := pkt.Dst == n.id.NodeID()
	direct := pkt.Flags&wire.FlagRelayed == 0

	switch {
	case pkt.IsBroadcast():
		n.consume(pkt, direct)
		n.relay(pkt, link)
	case forUs:
		n.consume(pkt, direct)
	default:
		n.relay(pkt, link)
	}
}

// consume handles a packet meant for us.
func (n *Node) consume(pkt *wire.Packet, direct bool) {
	switch pkt.Type {
	case wire.TypeAnnounce:
		n.handleAnnounce(pkt, direct)
	case wire.TypeHandshake:
		n.handleHandshake(pkt, direct)
	case wire.TypeData:
		n.handleData(pkt, direct)
	case wire.TypeAck:
		n.handleAck(pkt)
	default:
		n.log.Debug("unknown packet type", "type", pkt.Type)
	}
}

// handleAnnounce records presence. The announce is unauthenticated by
// necessity — it is how strangers become reachable at all — so it may only
// ever add a routing hint and a display name. Nothing it claims is trusted:
// the key it carries is proven when a handshake with that key succeeds, and
// until then a forged announce can misdirect a packet but never open one.
func (n *Node) handleAnnounce(pkt *wire.Packet, direct bool) {
	key, name, err := decodeAnnounce(pkt.Payload)
	if err != nil {
		n.log.Debug("bad announce", "err", err)
		return
	}
	if key.NodeID() != pkt.Src {
		// The announced key does not hash to the source it claims. Nothing
		// legitimate produces this.
		n.log.Debug("announce source mismatch", "claimed", pkt.Src)
		return
	}
	if key == n.id.Public() {
		return
	}

	n.mu.Lock()
	p := n.peers[key.NodeID()]
	if p == nil {
		p = &Peer{Key: key}
		n.peers[key.NodeID()] = p
	}
	p.Name = name
	p.LastSeen = n.now()
	p.Direct = direct
	p.Hops = int(wire.DefaultTTL - pkt.TTL)
	n.mu.Unlock()

	// Someone we were waiting on may have just appeared.
	n.flushCache()
	n.retryOutbox(false)
}

func (n *Node) handleHandshake(pkt *wire.Packet, direct bool) {
	res, err := n.sess.HandleHandshake(pkt.Payload)
	if err != nil {
		n.log.Debug("handshake failed", "src", pkt.Src, "err", err)
		return
	}

	// Learn the peer from the handshake. Unlike an announce, this key is
	// proven: only the holder of the private half could have produced it.
	n.mu.Lock()
	p := n.peers[res.Peer.NodeID()]
	if p == nil {
		p = &Peer{Key: res.Peer}
		n.peers[res.Peer.NodeID()] = p
	}
	p.LastSeen = n.now()
	n.mu.Unlock()

	if res.Reply != nil {
		n.sendPacket(&wire.Packet{
			Type:    wire.TypeHandshake,
			Dst:     res.Peer.NodeID(),
			Payload: res.Reply,
		})
	}

	// IK carries the first message inside the handshake, so a fresh session
	// often arrives with content already attached.
	if len(res.Payload) > 0 {
		msgID, payload, err := splitRef(res.Payload)
		if err == nil {
			n.deliver(res.Peer, payload, direct)
			n.sendAck(res.Peer.NodeID(), msgID)
		}
	}

	// A session existing is what unblocks anything queued for this peer.
	n.retryOutbox(true)
}

func (n *Node) handleData(pkt *wire.Packet, direct bool) {
	sess, ok := n.sess.Session(pkt.Src)
	if !ok {
		// They think we have a session and we do not, which happens whenever
		// one side restarts. We cannot decrypt this, but if we know their key
		// we can rebuild the session and they will resend.
		n.mu.Lock()
		p := n.peers[pkt.Src]
		n.mu.Unlock()
		if p != nil {
			n.openSession(p.Key, nil)
		}
		return
	}

	plain, err := sess.Open(pkt.Payload)
	if err != nil {
		if !errors.Is(err, session.ErrReplay) {
			n.log.Debug("could not open message", "src", pkt.Src, "err", err)
		}
		return
	}

	msgID, payload, err := splitRef(plain)
	if err != nil {
		n.log.Debug("malformed inner message", "err", err)
		return
	}

	n.deliver(sess.Peer(), payload, direct)
	n.sendAck(pkt.Src, msgID)
}

func (n *Node) handleAck(pkt *wire.Packet) {
	if len(pkt.Payload) != wire.MsgIDSize {
		return
	}
	var id wire.MsgID
	copy(id[:], pkt.Payload)

	n.mu.Lock()
	out, ok := n.outbox[id]
	if ok {
		delete(n.outbox, id)
	}
	n.mu.Unlock()

	if ok {
		n.report(Delivery{Ref: out.ref, Acked: true})
	}
}

func (n *Node) deliver(from identity.PublicKey, payload []byte, direct bool) {
	select {
	case n.inbound <- Inbound{From: from, Payload: payload, Direct: direct}:
	default:
		n.log.Warn("inbound queue full, dropping message", "from", from.NodeID())
	}
}

func (n *Node) report(d Delivery) {
	select {
	case n.delivered <- d:
	default:
	}
}

// relay forwards a packet that is not ours, or is a broadcast.
func (n *Node) relay(pkt *wire.Packet, from transport.LinkID) {
	if pkt.TTL == 0 {
		return
	}
	fwd := *pkt
	fwd.TTL--
	fwd.Flags |= wire.FlagRelayed

	raw, err := fwd.Encode()
	if err != nil {
		return
	}

	n.mu.Lock()
	// Prefer the link the destination was last heard on: in the common case of
	// two people in one room, this keeps their conversation off everyone
	// else's radio.
	target, unicast := n.routes[fwd.Dst]
	links := make([]transport.LinkID, 0, len(n.links))
	for l := range n.links {
		links = append(links, l)
	}
	n.mu.Unlock()

	if unicast && !fwd.IsBroadcast() && target != from {
		if err := n.tr.Send(target, raw); err == nil {
			return
		}
		// The shortcut failed, so fall through and flood.
	}

	sent := false
	for _, l := range links {
		if l == from {
			continue // never bounce a packet back where it came from
		}
		if err := n.tr.Send(l, raw); err == nil {
			sent = true
		}
	}

	// Nowhere to put it: hold it for whoever shows up next. This is what lets
	// a message cross a room where the two ends are never present at once.
	if !sent && !fwd.IsBroadcast() {
		n.hold(&fwd)
	}
}

// hold caches an undeliverable packet on behalf of its sender.
func (n *Node) hold(pkt *wire.Packet) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.cache) >= CacheLimit {
		// Drop the oldest: carrying a message that is minutes stale matters
		// less than having room for the one arriving now.
		n.cache = n.cache[1:]
	}
	cp := *pkt
	cp.Payload = append([]byte(nil), pkt.Payload...)
	n.cache = append(n.cache, cached{packet: &cp, deadline: n.now().Add(CacheTTL)})
}

// flushCache retries packets held for other people.
func (n *Node) flushCache() {
	n.mu.Lock()
	now := n.now()
	keep := n.cache[:0]
	var send []*wire.Packet
	for _, c := range n.cache {
		if now.After(c.deadline) {
			continue
		}
		if _, known := n.routes[c.packet.Dst]; known {
			send = append(send, c.packet)
			continue
		}
		keep = append(keep, c)
	}
	n.cache = keep
	n.mu.Unlock()

	for _, p := range send {
		raw, err := p.Encode()
		if err != nil {
			continue
		}
		n.mu.Lock()
		target, ok := n.routes[p.Dst]
		n.mu.Unlock()
		if ok {
			n.tr.Send(target, raw)
		}
	}
}

// tickLoop drives the periodic work: presence, retries and expiry.
func (n *Node) tickLoop() {
	defer n.wg.Done()

	announce := time.NewTicker(AnnounceInterval)
	retry := time.NewTicker(RetryInterval)
	defer announce.Stop()
	defer retry.Stop()

	for {
		select {
		case <-n.stop:
			return
		case <-announce.C:
			n.announce()
		case <-retry.C:
			n.retryOutbox(false)
			n.flushCache()
			n.expirePeers()
		}
	}
}

func (n *Node) announce() {
	payload := encodeAnnounce(n.id.Public(), n.name)
	n.sendPacket(&wire.Packet{Type: wire.TypeAnnounce, Payload: payload})
}

func (n *Node) expirePeers() {
	n.mu.Lock()
	defer n.mu.Unlock()
	cutoff := n.now().Add(-PeerExpiry)
	for id, p := range n.peers {
		if p.LastSeen.Before(cutoff) {
			delete(n.peers, id)
			delete(n.routes, id)
		}
	}
}

// retryOutbox re-attempts undelivered messages. When force is set the backoff
// is ignored, which is right after a new peer appears: the reason a message
// was stuck may have just gone away.
func (n *Node) retryOutbox(force bool) {
	n.mu.Lock()
	now := n.now()
	var due []*outgoing
	var failed []*outgoing
	for id, out := range n.outbox {
		if out.attempts >= MaxRetries {
			delete(n.outbox, id)
			failed = append(failed, out)
			continue
		}
		if force || !now.Before(out.next) {
			due = append(due, out)
		}
	}
	n.mu.Unlock()

	for _, out := range failed {
		n.report(Delivery{Ref: out.ref, Err: fmt.Errorf("mesh: gave up after %d attempts", out.attempts)})
	}
	for _, out := range due {
		n.attempt(out)
	}
}

// attempt tries to put one outgoing message on the air.
func (n *Node) attempt(out *outgoing) {
	n.mu.Lock()
	out.attempts++
	// Back off gently: peers reappear on human timescales, so there is no
	// value in hammering, and no value in waiting minutes either.
	out.next = n.now().Add(RetryInterval)
	needHandshake := !out.handshakeSent
	n.mu.Unlock()

	inner := joinRef(out.msgID, out.payload)
	to := out.to.NodeID()

	if sealed, err := n.sess.Seal(to, inner); err == nil {
		n.sendPacket(&wire.Packet{Type: wire.TypeData, Dst: to, ID: out.msgID, Payload: sealed})
		return
	}

	// No session. Open one, carrying this message as the handshake's payload
	// so a single packet both establishes the channel and delivers the note.
	if needHandshake {
		n.mu.Lock()
		out.handshakeSent = true
		n.mu.Unlock()
		n.openSession(out.to, inner)
	}
}

// openSession starts a Noise handshake, optionally with a 0-RTT payload.
func (n *Node) openSession(peer identity.PublicKey, payload []byte) {
	msg, err := n.sess.Initiate(peer, payload)
	if err != nil {
		n.log.Debug("could not start handshake", "peer", peer.NodeID(), "err", err)
		return
	}
	n.sendPacket(&wire.Packet{Type: wire.TypeHandshake, Dst: peer.NodeID(), Payload: msg})
}

func (n *Node) sendAck(to identity.NodeID, msgID wire.MsgID) {
	n.sendPacket(&wire.Packet{Type: wire.TypeAck, Dst: to, Payload: msgID[:]})
}

// sendPacket fills in the parts every outgoing packet shares and puts it on
// the air.
func (n *Node) sendPacket(pkt *wire.Packet) {
	pkt.Src = n.id.NodeID()
	pkt.TTL = wire.DefaultTTL
	if pkt.ID == (wire.MsgID{}) {
		id, err := wire.NewMsgID()
		if err != nil {
			return
		}
		pkt.ID = id
	}

	// Record our own id so a copy looping back through the mesh is dropped
	// rather than processed.
	n.seen.Mark(pkt.ID)

	raw, err := pkt.Encode()
	if err != nil {
		n.log.Debug("could not encode packet", "err", err)
		return
	}

	n.mu.Lock()
	target, unicast := n.routes[pkt.Dst]
	n.mu.Unlock()

	if unicast && !pkt.IsBroadcast() {
		if err := n.tr.Send(target, raw); err == nil {
			return
		}
	}

	if err := n.tr.Broadcast(raw); err != nil {
		n.log.Debug("broadcast incomplete", "err", err)
	}
	if !pkt.IsBroadcast() && n.LinkCount() == 0 {
		n.hold(pkt)
	}
}

// The inner envelope pairs a message id with its payload, so the recipient can
// acknowledge the exact message. It sits inside the encryption: a relay sees
// the outer packet id, never this one.

func joinRef(id wire.MsgID, payload []byte) []byte {
	out := make([]byte, wire.MsgIDSize+len(payload))
	copy(out, id[:])
	copy(out[wire.MsgIDSize:], payload)
	return out
}

func splitRef(b []byte) (wire.MsgID, []byte, error) {
	var id wire.MsgID
	if len(b) < wire.MsgIDSize {
		return id, nil, errors.New("mesh: inner message is too short")
	}
	copy(id[:], b[:wire.MsgIDSize])
	return id, b[wire.MsgIDSize:], nil
}

// An announce is a public key, a name length and a name.

func encodeAnnounce(key identity.PublicKey, name string) []byte {
	if len(name) > 64 {
		name = name[:64]
	}
	out := make([]byte, 0, identity.KeySize+2+len(name))
	out = append(out, key[:]...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(name)))
	return append(out, name...)
}

func decodeAnnounce(b []byte) (identity.PublicKey, string, error) {
	var key identity.PublicKey
	if len(b) < identity.KeySize+2 {
		return key, "", errors.New("announce is too short")
	}
	copy(key[:], b[:identity.KeySize])
	length := int(binary.BigEndian.Uint16(b[identity.KeySize:]))
	rest := b[identity.KeySize+2:]
	if len(rest) < length {
		return key, "", errors.New("announce name is truncated")
	}
	return key, string(rest[:length]), nil
}
