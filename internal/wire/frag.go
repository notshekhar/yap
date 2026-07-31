package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A BLE link carries frames of roughly 180 bytes, sometimes as few as 20 when
// a peer negotiates badly. Anything larger than one frame has to be split and
// put back together, and the reassembler has to assume the pieces may never
// all arrive — a peer walking out of range mid-message is the normal case
// here, not the exception.

const (
	// fragSolo prefixes a packet that fits in a single frame. One byte of
	// overhead, which matters: most chat messages are one frame, and paying a
	// nine-byte fragment header on every "ok" would be silly.
	fragSolo byte = 0

	// fragPart prefixes a piece of a split packet.
	fragPart byte = 1

	// FragHeaderSize is the prefix on a split frame:
	//   kind 1 | fragID 4 | index 2 | total 2
	FragHeaderSize = 1 + 4 + 2 + 2

	// SoloHeaderSize is the prefix on an unsplit frame.
	SoloHeaderSize = 1

	// MaxFragments caps how many pieces one packet may become, so a hostile
	// peer cannot claim a 65535-piece message and pin the memory while it
	// dribbles in.
	//
	// The cap is on pieces, not bytes, so what it permits depends on the link:
	// at a healthy 185-byte MTU it allows well over a megabyte, while a peer
	// stuck at BLE's 23-byte floor tops out around 90KB. That asymmetry is
	// intended. A link that bad would take an hour to move a megabyte anyway,
	// and refusing early beats discovering it at fragment 20,000.
	MaxFragments = 8192

	// MaxMessageBytes bounds a single reassembled packet. Fragment counting
	// alone does not bound memory, because a peer can send few but enormous
	// fragments on a link with a large MTU.
	MaxMessageBytes = 1 << 20
)

// ErrMTUTooSmall means the link cannot carry even one byte of payload.
var ErrMTUTooSmall = errors.New("wire: mtu too small to carry a fragment")

// Split cuts an encoded packet into link frames of at most mtu bytes.
func Split(packet []byte, mtu int, fragID uint32) ([][]byte, error) {
	if mtu <= SoloHeaderSize {
		return nil, fmt.Errorf("%w: %d", ErrMTUTooSmall, mtu)
	}

	if len(packet)+SoloHeaderSize <= mtu {
		frame := make([]byte, 0, len(packet)+SoloHeaderSize)
		frame = append(frame, fragSolo)
		frame = append(frame, packet...)
		return [][]byte{frame}, nil
	}

	chunkSize := mtu - FragHeaderSize
	if chunkSize <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrMTUTooSmall, mtu)
	}

	total := (len(packet) + chunkSize - 1) / chunkSize
	if total > MaxFragments {
		return nil, fmt.Errorf("wire: packet needs %d fragments, limit is %d", total, MaxFragments)
	}

	frames := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		start := i * chunkSize
		end := min(start+chunkSize, len(packet))

		frame := make([]byte, FragHeaderSize+(end-start))
		frame[0] = fragPart
		binary.BigEndian.PutUint32(frame[1:], fragID)
		binary.BigEndian.PutUint16(frame[5:], uint16(i))
		binary.BigEndian.PutUint16(frame[7:], uint16(total))
		copy(frame[FragHeaderSize:], packet[start:end])
		frames = append(frames, frame)
	}
	return frames, nil
}

type pending struct {
	parts    [][]byte
	have     int
	total    int
	size     int
	deadline time.Time
}

// Reassembler rebuilds packets from link frames. One per link: fragment IDs
// are only unique per sender, so mixing links would let two peers collide.
type Reassembler struct {
	mu      sync.Mutex
	partial map[uint32]*pending

	// TTL bounds how long a half-arrived message is held. A peer that walks
	// away mid-send would otherwise leak its partial buffers forever.
	TTL time.Duration

	// MaxPending caps concurrent half-arrived messages, so a peer cannot open
	// thousands of fragment IDs and exhaust memory without ever completing one.
	MaxPending int

	now func() time.Time
}

// NewReassembler returns a reassembler with sane bounds.
func NewReassembler() *Reassembler {
	return &Reassembler{
		partial:    make(map[uint32]*pending),
		TTL:        30 * time.Second,
		MaxPending: 64,
		now:        time.Now,
	}
}

// Push feeds one received frame. It returns a complete packet when the frame
// finished one, or nil when more frames are still needed.
func (r *Reassembler) Push(frame []byte) ([]byte, error) {
	if len(frame) < SoloHeaderSize {
		return nil, ErrShort
	}

	switch frame[0] {
	case fragSolo:
		out := make([]byte, len(frame)-SoloHeaderSize)
		copy(out, frame[SoloHeaderSize:])
		return out, nil

	case fragPart:
		if len(frame) < FragHeaderSize {
			return nil, ErrShort
		}
		return r.pushPart(frame)

	default:
		return nil, fmt.Errorf("wire: unknown frame kind %d", frame[0])
	}
}

func (r *Reassembler) pushPart(frame []byte) ([]byte, error) {
	var (
		id    = binary.BigEndian.Uint32(frame[1:])
		index = int(binary.BigEndian.Uint16(frame[5:]))
		total = int(binary.BigEndian.Uint16(frame[7:]))
		chunk = frame[FragHeaderSize:]
	)

	if total == 0 || total > MaxFragments {
		return nil, fmt.Errorf("wire: fragment claims %d parts", total)
	}
	if index >= total {
		return nil, fmt.Errorf("wire: fragment %d of %d is out of range", index, total)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()

	p := r.partial[id]
	if p == nil {
		if len(r.partial) >= r.MaxPending {
			return nil, fmt.Errorf("wire: too many half-received messages (%d)", len(r.partial))
		}
		p = &pending{parts: make([][]byte, total), total: total}
		r.partial[id] = p
	}
	if p.total != total {
		// The sender reused a fragment ID with a different shape. Treat the
		// old one as abandoned rather than splicing two messages together.
		p = &pending{parts: make([][]byte, total), total: total}
		r.partial[id] = p
	}
	p.deadline = r.now().Add(r.TTL)

	if p.parts[index] == nil {
		if p.size+len(chunk) > MaxMessageBytes {
			delete(r.partial, id)
			return nil, fmt.Errorf("wire: message exceeds %d bytes", MaxMessageBytes)
		}
		stored := make([]byte, len(chunk))
		copy(stored, chunk)
		p.parts[index] = stored
		p.have++
		p.size += len(stored)
	}

	if p.have < p.total {
		return nil, nil
	}

	out := make([]byte, 0, p.size)
	for _, part := range p.parts {
		out = append(out, part...)
	}
	delete(r.partial, id)
	return out, nil
}

// Pending reports how many messages are half-received, for tests and status.
func (r *Reassembler) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	return len(r.partial)
}

func (r *Reassembler) expireLocked() {
	now := r.now()
	for id, p := range r.partial {
		if now.After(p.deadline) {
			delete(r.partial, id)
		}
	}
}
