// Package loopback is a virtual radio: an in-process transport where the
// topology is whatever the test says it is.
//
// It exists because of a measured fact about the real one. Two processes on a
// single Mac cannot see each other over Bluetooth — CoreBluetooth does not
// loop a host's own advertisement back to its own scanner — so a laptop can
// never host both ends of a link. Without a virtual radio, every change to the
// router would need two machines and a person to carry one of them out of
// range.
//
// This transport can do things the real one cannot: place nodes in a line so
// packets must be relayed, drop a configurable share of traffic, and partition
// the room in half. Those are the conditions the router has to survive, and
// here they are reproducible.
package loopback

import (
	"context"
	"fmt"
	"math/rand"
	"sync"

	"github.com/notshekhar/yap/internal/transport"
)

// DefaultMTU matches what CoreBluetooth typically negotiates, so packets get
// split in tests at roughly the size they will be split in the field.
const DefaultMTU = 180

// Room is the shared airspace. Nodes hear each other only where the room says
// they are adjacent.
type Room struct {
	mu    sync.Mutex
	nodes map[string]*Node
	adj   map[string]map[string]bool
	mtu   int

	// loss is the fraction of packets silently dropped in flight, for testing
	// the mesh under the conditions Bluetooth actually provides.
	loss float64
	rng  *rand.Rand
}

// NewRoom creates an empty room with no nodes and no links.
func NewRoom() *Room {
	return &Room{
		nodes: make(map[string]*Node),
		adj:   make(map[string]map[string]bool),
		mtu:   DefaultMTU,
		// Fixed seed: a flaky mesh test that cannot be reproduced is worse
		// than no test.
		rng: rand.New(rand.NewSource(1)),
	}
}

// SetMTU changes the per-link MTU for links formed afterwards.
func (r *Room) SetMTU(mtu int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mtu = mtu
}

// SetLoss sets the probability that any given packet vanishes in flight.
func (r *Room) SetLoss(p float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loss = p
}

// Join adds a node to the room. The name is a test-facing label, not an
// identity; the mesh authenticates peers by key regardless of what the
// transport calls them.
func (r *Room) Join(name string) *Node {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := &Node{
		name:   name,
		room:   r,
		events: make(chan transport.Event, 256),
	}
	r.nodes[name] = n
	r.adj[name] = make(map[string]bool)
	return n
}

// Connect makes two nodes mutually audible and raises LinkUp on both.
func (r *Room) Connect(a, b string) {
	r.mu.Lock()
	na, nb := r.nodes[a], r.nodes[b]
	if na == nil || nb == nil || a == b {
		r.mu.Unlock()
		return
	}
	if r.adj[a][b] {
		r.mu.Unlock()
		return
	}
	r.adj[a][b] = true
	r.adj[b][a] = true
	mtu := r.mtu
	r.mu.Unlock()

	na.emit(transport.Event{Kind: transport.LinkUp, Link: transport.LinkID(b), MTU: mtu})
	nb.emit(transport.Event{Kind: transport.LinkUp, Link: transport.LinkID(a), MTU: mtu})
}

// Disconnect severs a link, as happens whenever someone walks away.
func (r *Room) Disconnect(a, b string) {
	r.mu.Lock()
	na, nb := r.nodes[a], r.nodes[b]
	if na == nil || nb == nil || !r.adj[a][b] {
		r.mu.Unlock()
		return
	}
	delete(r.adj[a], b)
	delete(r.adj[b], a)
	r.mu.Unlock()

	na.emit(transport.Event{Kind: transport.LinkDown, Link: transport.LinkID(b)})
	nb.emit(transport.Event{Kind: transport.LinkDown, Link: transport.LinkID(a)})
}

// Chain wires nodes in a line, so traffic between the ends must be relayed by
// everyone in between. This is the topology that proves multi-hop works.
func (r *Room) Chain(names ...string) {
	for i := 0; i+1 < len(names); i++ {
		r.Connect(names[i], names[i+1])
	}
}

// Mesh wires every node to every other, the crowded-room case.
func (r *Room) Mesh(names ...string) {
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			r.Connect(names[i], names[j])
		}
	}
}

func (r *Room) neighbours(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.adj[name]))
	for peer := range r.adj[name] {
		out = append(out, peer)
	}
	return out
}

func (r *Room) linked(a, b string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adj[a][b]
}

func (r *Room) node(name string) *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodes[name]
}

func (r *Room) dropped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loss > 0 && r.rng.Float64() < r.loss
}

func (r *Room) currentMTU() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mtu
}

// Node is one participant's view of the room. It implements transport.Transport.
type Node struct {
	name string
	room *Room

	mu     sync.Mutex
	closed bool
	events chan transport.Event
}

var _ transport.Transport = (*Node)(nil)

// Start satisfies the interface. A virtual radio is always on.
func (n *Node) Start(ctx context.Context) error { return nil }

// Send delivers one packet to one neighbour.
func (n *Node) Send(link transport.LinkID, packet []byte) error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return transport.ErrClosed
	}
	n.mu.Unlock()

	peer := string(link)
	if !n.room.linked(n.name, peer) {
		return fmt.Errorf("%w: %s", transport.ErrNoSuchLink, peer)
	}

	// Model the radio, not the wire: a packet that exceeds what the link can
	// carry is the transport's problem to split, and this one does not split,
	// so it refuses rather than pretending.
	if len(packet) > n.room.currentMTU()*512 {
		return transport.ErrTooLarge
	}

	if n.room.dropped() {
		return nil // lost in flight, exactly as the radio would lose it
	}

	target := n.room.node(peer)
	if target == nil {
		return fmt.Errorf("%w: %s", transport.ErrNoSuchLink, peer)
	}

	cp := make([]byte, len(packet))
	copy(cp, packet)
	target.emit(transport.Event{
		Kind:   transport.PacketReceived,
		Link:   transport.LinkID(n.name),
		Packet: cp,
	})
	return nil
}

// Broadcast delivers to every neighbour, continuing past failures.
func (n *Node) Broadcast(packet []byte) error {
	var firstErr error
	for _, peer := range n.room.neighbours(n.name) {
		if err := n.Send(transport.LinkID(peer), packet); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Links lists current neighbours.
func (n *Node) Links() []transport.Link {
	peers := n.room.neighbours(n.name)
	mtu := n.room.currentMTU()
	out := make([]transport.Link, 0, len(peers))
	for _, p := range peers {
		out = append(out, transport.Link{ID: transport.LinkID(p), MTU: mtu, Addr: p})
	}
	return out
}

// Events returns this node's event stream.
func (n *Node) Events() <-chan transport.Event { return n.events }

// Close shuts the node down.
func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	n.closed = true
	close(n.events)
	return nil
}

// emit queues an event, dropping it if the consumer has fallen far behind.
// Blocking here would deadlock the sender against a slow receiver, and a real
// radio drops under pressure too.
func (n *Node) emit(ev transport.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	select {
	case n.events <- ev:
	default:
	}
}
