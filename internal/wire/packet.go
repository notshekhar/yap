// Package wire is the on-air format: what a yap node actually puts on a
// Bluetooth link.
//
// Everything here is deliberately readable by any relay, because a relay has
// to route it. The rule that keeps that honest is that this layer never sees
// plaintext — Payload on a Data packet is already a sealed Noise ciphertext by
// the time it arrives. A relay learns that some node talked to some other node
// and how big it was. It cannot learn what was said.
package wire

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/notshekhar/yap/internal/identity"
)

// Version is the current on-air format. A node that sees anything else drops
// the packet rather than guessing.
const Version = 1

// HeaderSize is the fixed prefix on every packet.
//
//	version 1 | type 1 | ttl 1 | flags 1 | msgID 8 | src 8 | dst 8 | len 2
const HeaderSize = 1 + 1 + 1 + 1 + MsgIDSize + identity.NodeIDSize + identity.NodeIDSize + 2

// MsgIDSize is the width of the random per-packet identifier used for dedupe.
const MsgIDSize = 8

// MaxPayload is bounded by the 16-bit length field.
const MaxPayload = 65535

// DefaultTTL is how many relays a packet may traverse. Seven hops of Bluetooth
// is already a large room; beyond that the flood costs more than it delivers.
const DefaultTTL = 7

// Type distinguishes what a packet is for.
type Type uint8

const (
	// TypeAnnounce broadcasts presence: "this key, this name, is nearby".
	// Sent in the clear because it is how strangers find each other at all.
	TypeAnnounce Type = 1

	// TypeHandshake carries a Noise IK handshake message.
	TypeHandshake Type = 2

	// TypeData carries an encrypted application payload.
	TypeData Type = 3

	// TypeAck confirms receipt of a Data packet to its sender.
	TypeAck Type = 4
)

func (t Type) String() string {
	switch t {
	case TypeAnnounce:
		return "announce"
	case TypeHandshake:
		return "handshake"
	case TypeData:
		return "data"
	case TypeAck:
		return "ack"
	default:
		return fmt.Sprintf("type(%d)", uint8(t))
	}
}

// Flags carries single-bit hints that relays may act on.
type Flags uint8

const (
	// FlagRelayed is set once a packet has been forwarded at least once, so a
	// receiver can tell a direct neighbour from a distant peer.
	FlagRelayed Flags = 1 << 0
)

// MsgID is a random per-packet identifier. Dedupe is keyed on it, so it must
// be random rather than sequential: a counter would leak how much a node has
// sent and would collide across restarts.
type MsgID [MsgIDSize]byte

// NewMsgID draws a fresh identifier.
func NewMsgID() (MsgID, error) {
	var id MsgID
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("draw message id: %w", err)
	}
	return id, nil
}

// Packet is one routable unit.
type Packet struct {
	Type    Type
	TTL     uint8
	Flags   Flags
	ID      MsgID
	Src     identity.NodeID
	Dst     identity.NodeID // zero means broadcast
	Payload []byte
}

// IsBroadcast reports whether the packet is addressed to everyone.
func (p *Packet) IsBroadcast() bool { return p.Dst.IsZero() }

// Encode serialises the packet.
func (p *Packet) Encode() ([]byte, error) {
	if len(p.Payload) > MaxPayload {
		return nil, fmt.Errorf("payload is %d bytes, limit is %d", len(p.Payload), MaxPayload)
	}

	buf := make([]byte, HeaderSize+len(p.Payload))
	buf[0] = Version
	buf[1] = byte(p.Type)
	buf[2] = p.TTL
	buf[3] = byte(p.Flags)

	n := 4
	n += copy(buf[n:], p.ID[:])
	n += copy(buf[n:], p.Src[:])
	n += copy(buf[n:], p.Dst[:])
	binary.BigEndian.PutUint16(buf[n:], uint16(len(p.Payload)))
	n += 2
	copy(buf[n:], p.Payload)
	return buf, nil
}

var (
	// ErrShort means the buffer cannot hold a packet at all.
	ErrShort = errors.New("wire: buffer shorter than header")
	// ErrVersion means the packet came from an incompatible build.
	ErrVersion = errors.New("wire: unsupported version")
	// ErrTruncated means the declared length exceeds what arrived.
	ErrTruncated = errors.New("wire: payload truncated")
)

// Decode parses a packet. The returned payload aliases nothing in buf, so the
// caller may reuse the read buffer immediately — a real concern given frames
// arrive on a shared BLE callback path.
func Decode(buf []byte) (*Packet, error) {
	if len(buf) < HeaderSize {
		return nil, ErrShort
	}
	if buf[0] != Version {
		return nil, fmt.Errorf("%w: %d", ErrVersion, buf[0])
	}

	p := &Packet{
		Type:  Type(buf[1]),
		TTL:   buf[2],
		Flags: Flags(buf[3]),
	}

	n := 4
	n += copy(p.ID[:], buf[n:])
	n += copy(p.Src[:], buf[n:])
	n += copy(p.Dst[:], buf[n:])
	length := int(binary.BigEndian.Uint16(buf[n:]))
	n += 2

	if len(buf)-n < length {
		return nil, fmt.Errorf("%w: declared %d, have %d", ErrTruncated, length, len(buf)-n)
	}
	if length > 0 {
		p.Payload = make([]byte, length)
		copy(p.Payload, buf[n:n+length])
	}
	return p, nil
}
