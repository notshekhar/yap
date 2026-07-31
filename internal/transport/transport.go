// Package transport is the seam between the mesh and the radio.
//
// The mesh router deals in whole packets and knows nothing about MTUs,
// CoreBluetooth delegates or connection churn. A transport deals in links to
// nearby nodes and hides everything specific to how those links exist. That
// separation is what makes the router testable: proving multi-hop relay works
// should not require three Macs and a hallway.
package transport

import (
	"context"
	"errors"
)

// LinkID identifies one connection to one nearby node. It is assigned by the
// transport and is meaningful only to that transport.
//
// A link is not an identity. On macOS the underlying handle is a per-host
// CoreBluetooth UUID that changes between machines and reboots, and any peer
// can present whatever it likes at this layer. Who is on the other end is
// settled by the Noise handshake, never by the link.
type LinkID string

// Kind describes what happened on a transport.
type Kind int

const (
	// LinkUp means a new link is usable.
	LinkUp Kind = iota

	// LinkDown means a link is gone. Any session state keyed to it stays
	// valid — peers reconnect constantly as people move around, and tearing
	// down encryption on every flap would mean re-handshaking all day.
	LinkDown

	// PacketReceived carries one complete packet.
	PacketReceived
)

// Event is something a transport observed.
type Event struct {
	Kind Kind
	Link LinkID

	// MTU is the usable payload per frame on this link, set on LinkUp.
	MTU int

	// Packet is a complete, reassembled packet, set on PacketReceived. The
	// receiver owns it.
	Packet []byte
}

// Link describes a live connection.
type Link struct {
	ID  LinkID
	MTU int

	// Addr is a human-readable hint about the peer, for diagnostics only. It
	// is unauthenticated and must never drive a decision.
	Addr string
}

// Transport moves whole packets between nearby nodes.
//
// Implementations handle their own fragmentation: MTU is a property of the
// link, so the layer that owns the link owns the splitting.
type Transport interface {
	// Start begins discovery and accepting connections. It returns once the
	// transport is running; it does not block for the transport's lifetime.
	Start(ctx context.Context) error

	// Send delivers one packet over one link.
	Send(link LinkID, packet []byte) error

	// Broadcast delivers a packet over every live link. It reports the first
	// error but always attempts every link: on a mesh, one failing peer must
	// not stop the flood reaching the rest.
	Broadcast(packet []byte) error

	// Links lists what is currently reachable.
	Links() []Link

	// Events returns the stream of transport events. It is closed when the
	// transport shuts down.
	Events() <-chan Event

	// Close stops the transport and releases the radio.
	Close() error
}

// Errors a transport may return.
var (
	// ErrNoSuchLink means the link is gone, usually because the peer moved out
	// of range between the router choosing it and the send happening.
	ErrNoSuchLink = errors.New("transport: no such link")

	// ErrClosed means the transport has shut down.
	ErrClosed = errors.New("transport: closed")

	// ErrTooLarge means the packet exceeds what this transport can carry.
	ErrTooLarge = errors.New("transport: packet too large for link")
)
