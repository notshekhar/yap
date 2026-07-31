package wire

import (
	"bytes"
	"math/rand"
	"testing"
	"time"

	"github.com/notshekhar/yap/internal/identity"
)

func testPacket(t *testing.T, payload []byte) *Packet {
	t.Helper()
	id, err := NewMsgID()
	if err != nil {
		t.Fatal(err)
	}
	a, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	return &Packet{
		Type:    TypeData,
		TTL:     DefaultTTL,
		ID:      id,
		Src:     a.NodeID(),
		Dst:     b.NodeID(),
		Payload: payload,
	}
}

func TestPacketRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 31, 200, 4096, MaxPayload} {
		payload := make([]byte, size)
		rand.Read(payload)

		p := testPacket(t, payload)
		enc, err := p.Encode()
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if len(enc) != HeaderSize+size {
			t.Fatalf("size %d: encoded to %d bytes", size, len(enc))
		}

		got, err := Decode(enc)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if got.Type != p.Type || got.TTL != p.TTL || got.ID != p.ID || got.Src != p.Src || got.Dst != p.Dst {
			t.Fatalf("size %d: header did not survive", size)
		}
		if !bytes.Equal(got.Payload, payload) {
			t.Fatalf("size %d: payload did not survive", size)
		}
	}
}

// Frames arrive on a shared BLE callback buffer that the transport is free to
// reuse the moment we return. If Decode aliased it, payloads would corrupt
// under load in a way that is near impossible to debug later.
func TestDecodeCopiesPayload(t *testing.T) {
	p := testPacket(t, []byte("original secret"))
	enc, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}

	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	for i := range enc {
		enc[i] = 0xff
	}
	if string(got.Payload) != "original secret" {
		t.Fatalf("payload aliased the input buffer: %q", got.Payload)
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	p := testPacket(t, []byte("hello"))
	good, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("short", func(t *testing.T) {
		if _, err := Decode(good[:HeaderSize-1]); err != ErrShort {
			t.Fatalf("want ErrShort, got %v", err)
		}
	})
	t.Run("version", func(t *testing.T) {
		bad := bytes.Clone(good)
		bad[0] = 99
		if _, err := Decode(bad); err == nil {
			t.Fatal("accepted unknown version")
		}
	})
	t.Run("truncated payload", func(t *testing.T) {
		if _, err := Decode(good[:len(good)-2]); err == nil {
			t.Fatal("accepted truncated payload")
		}
	})
}

func TestBroadcastDetection(t *testing.T) {
	p := testPacket(t, nil)
	if p.IsBroadcast() {
		t.Fatal("addressed packet reported as broadcast")
	}
	p.Dst = identity.NodeID{}
	if !p.IsBroadcast() {
		t.Fatal("zero destination not treated as broadcast")
	}
}

func TestSplitAndReassemble(t *testing.T) {
	// 20 is about the worst MTU a real peer negotiates; 512 is the best.
	for _, mtu := range []int{20, 64, 185, 512} {
		for _, size := range []int{0, 10, 500, 5000, 40000} {
			payload := make([]byte, size)
			rand.Read(payload)
			packet, err := testPacket(t, payload).Encode()
			if err != nil {
				t.Fatal(err)
			}

			frames, err := Split(packet, mtu, 7)
			if err != nil {
				t.Fatalf("mtu %d size %d: %v", mtu, size, err)
			}
			for i, f := range frames {
				if len(f) > mtu {
					t.Fatalf("mtu %d: frame %d is %d bytes, over the limit", mtu, i, len(f))
				}
			}

			r := NewReassembler()
			var out []byte
			for _, f := range frames {
				got, err := r.Push(f)
				if err != nil {
					t.Fatalf("mtu %d size %d: %v", mtu, size, err)
				}
				if got != nil {
					out = got
				}
			}
			if !bytes.Equal(out, packet) {
				t.Fatalf("mtu %d size %d: reassembled %d bytes, want %d", mtu, size, len(out), len(packet))
			}
			if r.Pending() != 0 {
				t.Fatalf("mtu %d size %d: %d messages left pending", mtu, size, r.Pending())
			}
		}
	}
}

// A short message must not pay a fragment header it does not need.
func TestSmallPacketUsesSoloFrame(t *testing.T) {
	packet, err := testPacket(t, []byte("hey")).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := Split(packet, 185, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames for a short message", len(frames))
	}
	if overhead := len(frames[0]) - len(packet); overhead != SoloHeaderSize {
		t.Fatalf("solo frame overhead is %d bytes, want %d", overhead, SoloHeaderSize)
	}
}

// BLE does not promise ordering across a reconnect, and relays interleave.
func TestReassembleOutOfOrderAndDuplicated(t *testing.T) {
	packet, err := testPacket(t, bytes.Repeat([]byte("x"), 3000)).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := Split(packet, 100, 42)
	if err != nil {
		t.Fatal(err)
	}

	shuffled := append([][]byte{}, frames...)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	// Duplicate every frame: a repeated fragment must not double-count toward
	// completion, or the message completes early and decodes as garbage.
	shuffled = append(shuffled, frames...)

	r := NewReassembler()
	var out []byte
	for _, f := range shuffled {
		got, err := r.Push(f)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil && out == nil {
			out = got
		}
	}
	if !bytes.Equal(out, packet) {
		t.Fatal("out-of-order reassembly produced the wrong bytes")
	}
}

func TestReassemblerDropsAbandonedMessages(t *testing.T) {
	packet, err := testPacket(t, bytes.Repeat([]byte("y"), 2000)).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := Split(packet, 100, 9)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	r := NewReassembler()
	r.TTL = time.Minute
	r.now = func() time.Time { return now }

	// A peer sends half a message and walks out of range.
	for _, f := range frames[:len(frames)/2] {
		if _, err := r.Push(f); err != nil {
			t.Fatal(err)
		}
	}
	if r.Pending() != 1 {
		t.Fatalf("want 1 pending, got %d", r.Pending())
	}

	now = now.Add(2 * time.Minute)
	if r.Pending() != 0 {
		t.Fatalf("abandoned message was not expired: %d pending", r.Pending())
	}
}

func TestReassemblerRejectsHostileFragments(t *testing.T) {
	r := NewReassembler()

	t.Run("index out of range", func(t *testing.T) {
		f := make([]byte, FragHeaderSize+4)
		f[0] = fragPart
		f[5], f[6] = 0, 9 // index 9
		f[7], f[8] = 0, 2 // of 2
		if _, err := r.Push(f); err == nil {
			t.Fatal("accepted a fragment index past the total")
		}
	})

	t.Run("absurd total", func(t *testing.T) {
		f := make([]byte, FragHeaderSize+4)
		f[0] = fragPart
		f[7], f[8] = 0xff, 0xff // 65535 parts
		if _, err := r.Push(f); err == nil {
			t.Fatal("accepted a fragment count over the cap")
		}
	})

	t.Run("pending flood", func(t *testing.T) {
		r := NewReassembler()
		r.MaxPending = 4
		var lastErr error
		for i := 0; i < 100; i++ {
			f := make([]byte, FragHeaderSize+4)
			f[0] = fragPart
			f[1], f[2], f[3], f[4] = byte(i>>24), byte(i>>16), byte(i>>8), byte(i)
			f[7], f[8] = 0, 2 // always claims 2 parts, never sends the second
			if _, err := r.Push(f); err != nil {
				lastErr = err
			}
		}
		if lastErr == nil {
			t.Fatal("a peer opened unbounded half-messages without being refused")
		}
		if r.Pending() > 4 {
			t.Fatalf("pending grew to %d despite the cap", r.Pending())
		}
	})
}

func TestSeenDeduplicates(t *testing.T) {
	s := NewSeen(1000, time.Minute)
	id, err := NewMsgID()
	if err != nil {
		t.Fatal(err)
	}

	if !s.Mark(id) {
		t.Fatal("first sighting reported as duplicate")
	}
	for i := 0; i < 10; i++ {
		if s.Mark(id) {
			t.Fatal("repeat sighting reported as new")
		}
	}
}

func TestSeenEvictsOldest(t *testing.T) {
	const max = 8
	s := NewSeen(max, time.Hour)

	var ids []MsgID
	for i := 0; i < max*3; i++ {
		id, err := NewMsgID()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		s.Mark(id)
	}

	if s.Len() > max {
		t.Fatalf("held %d ids, cap is %d", s.Len(), max)
	}
	// The newest must still be remembered; the oldest must have been dropped.
	if s.Mark(ids[len(ids)-1]) {
		t.Fatal("newest id was forgotten")
	}
	if !s.Mark(ids[0]) {
		t.Fatal("oldest id was retained past the cap")
	}
}

func TestSeenExpiresByTime(t *testing.T) {
	now := time.Now()
	s := NewSeen(1000, time.Minute)
	s.now = func() time.Time { return now }

	id, err := NewMsgID()
	if err != nil {
		t.Fatal(err)
	}
	s.Mark(id)
	if s.Mark(id) {
		t.Fatal("id was not remembered at all")
	}

	now = now.Add(2 * time.Minute)
	if !s.Mark(id) {
		t.Fatal("id was still remembered past its ttl")
	}
}

// A peer must not be able to pin an id in the table forever by resending it.
func TestSeenTTLIsNotRefreshedByRepeats(t *testing.T) {
	now := time.Now()
	s := NewSeen(1000, time.Minute)
	s.now = func() time.Time { return now }

	id, err := NewMsgID()
	if err != nil {
		t.Fatal(err)
	}
	s.Mark(id)

	// Probe repeatedly while the entry is still live. Each of these is a
	// duplicate, and none of them may push the deadline out.
	for i := 0; i < 5; i++ {
		now = now.Add(10 * time.Second)
		if s.Mark(id) {
			t.Fatalf("entry expired early, at +%ds", (i+1)*10)
		}
	}

	// 61s against a 60s ttl: it must have lapsed on its original clock rather
	// than on the clock of the last sighting.
	now = now.Add(11 * time.Second)
	if !s.Mark(id) {
		t.Fatal("repeated sightings kept the entry alive past its ttl")
	}
}
