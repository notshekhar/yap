// Package tcpx carries the mesh over TCP instead of Bluetooth.
//
// It exists for two reasons, one practical and one structural.
//
// The practical one is measured: two processes on a single Mac cannot see each
// other over BLE, because CoreBluetooth does not loop a host's own
// advertisement back to its own scanner. Without another transport, every
// end-to-end change would need two machines and somebody to walk between them.
// Over TCP the same mesh, the same handshake and the same encryption can be
// exercised on one laptop in a second.
//
// The structural one is that it proves the seam is real. The mesh above has no
// idea which of these is underneath it, and the fact that both work unchanged
// is what makes a Linux or Wi-Fi Direct port a day of work rather than a
// rewrite.
//
// It is not a privacy story: TCP links are unencrypted at this layer. They do
// not need to be. The payloads are already sealed by the session layer before
// they arrive, exactly as they are on the radio, so a TCP relay learns no more
// than a Bluetooth one.
package tcpx

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/notshekhar/yap/internal/transport"
)

const (
	// maxFrame bounds a single packet so a hostile or broken peer cannot
	// convince us to allocate arbitrarily.
	maxFrame = 1 << 20

	// dialInterval is how often we retry peers that are not currently up.
	// People restart nodes constantly; the mesh should re-form without anyone
	// having to do anything.
	dialInterval = 5 * time.Second

	// mtu here is a fiction: TCP is a stream and needs no fragmenting. It is
	// reported large so the mesh does not split packets it does not need to.
	mtu = 1 << 16
)

// Transport is a TCP-backed mesh link layer.
type Transport struct {
	log   *slog.Logger
	seeds []string

	mu    sync.Mutex
	conns map[transport.LinkID]net.Conn

	events chan transport.Event
	ln     net.Listener

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

var _ transport.Transport = (*Transport)(nil)

// New creates a TCP transport that listens on addr and dials each seed peer.
func New(addr string, seeds []string, log *slog.Logger) (*Transport, error) {
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return &Transport{
		log:    log,
		seeds:  seeds,
		conns:  make(map[transport.LinkID]net.Conn),
		events: make(chan transport.Event, 512),
		ln:     ln,
		stop:   make(chan struct{}),
	}, nil
}

// Addr reports where this transport is listening, which matters when the
// requested port was taken.
func (t *Transport) Addr() string { return t.ln.Addr().String() }

// Start begins accepting and dialling.
func (t *Transport) Start(ctx context.Context) error {
	t.wg.Add(2)
	go t.accept()
	go t.dialLoop()
	return nil
}

func (t *Transport) accept() {
	defer t.wg.Done()
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			select {
			case <-t.stop:
				return
			default:
			}
			continue
		}
		t.adopt(conn, "in:"+conn.RemoteAddr().String())
	}
}

// dialLoop keeps trying to reach every configured peer. A peer that is down
// now may be up in a minute, and nobody should have to restart anything.
func (t *Transport) dialLoop() {
	defer t.wg.Done()

	tick := time.NewTicker(dialInterval)
	defer tick.Stop()

	t.dialAll()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			t.dialAll()
		}
	}
}

func (t *Transport) dialAll() {
	for _, seed := range t.seeds {
		id := transport.LinkID("out:" + seed)

		t.mu.Lock()
		_, up := t.conns[id]
		t.mu.Unlock()
		if up {
			continue
		}

		conn, err := net.DialTimeout("tcp", seed, 3*time.Second)
		if err != nil {
			continue
		}
		t.adopt(conn, string(id))
	}
}

func (t *Transport) adopt(conn net.Conn, id string) {
	link := transport.LinkID(id)

	t.mu.Lock()
	if old, ok := t.conns[link]; ok {
		old.Close()
	}
	t.conns[link] = conn
	t.mu.Unlock()

	t.emit(transport.Event{Kind: transport.LinkUp, Link: link, MTU: mtu})

	t.wg.Add(1)
	go t.read(link, conn)
}

func (t *Transport) read(link transport.LinkID, conn net.Conn) {
	defer t.wg.Done()
	defer func() {
		conn.Close()
		t.mu.Lock()
		if t.conns[link] == conn {
			delete(t.conns, link)
		}
		t.mu.Unlock()
		t.emit(transport.Event{Kind: transport.LinkDown, Link: link})
	}()

	var header [4]byte
	for {
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(header[:])
		if n == 0 || n > maxFrame {
			t.log.Debug("tcpx: refusing frame", "bytes", n, "link", link)
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		t.emit(transport.Event{Kind: transport.PacketReceived, Link: link, Packet: buf})
	}
}

// Send writes one packet to one link.
func (t *Transport) Send(link transport.LinkID, packet []byte) error {
	if len(packet) > maxFrame {
		return transport.ErrTooLarge
	}

	t.mu.Lock()
	conn, ok := t.conns[link]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", transport.ErrNoSuchLink, link)
	}

	frame := make([]byte, 4+len(packet))
	binary.BigEndian.PutUint32(frame, uint32(len(packet)))
	copy(frame[4:], packet)

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(frame); err != nil {
		conn.Close()
		return err
	}
	return nil
}

// Broadcast writes to every link, continuing past failures.
func (t *Transport) Broadcast(packet []byte) error {
	t.mu.Lock()
	links := make([]transport.LinkID, 0, len(t.conns))
	for l := range t.conns {
		links = append(links, l)
	}
	t.mu.Unlock()

	var firstErr error
	for _, l := range links {
		if err := t.Send(l, packet); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Links lists live connections.
func (t *Transport) Links() []transport.Link {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]transport.Link, 0, len(t.conns))
	for id, conn := range t.conns {
		out = append(out, transport.Link{ID: id, MTU: mtu, Addr: conn.RemoteAddr().String()})
	}
	return out
}

// Events returns the event stream.
func (t *Transport) Events() <-chan transport.Event { return t.events }

// Close shuts the transport down.
func (t *Transport) Close() error {
	t.stopOnce.Do(func() {
		close(t.stop)
		t.ln.Close()

		t.mu.Lock()
		for _, conn := range t.conns {
			conn.Close()
		}
		t.conns = map[transport.LinkID]net.Conn{}
		t.mu.Unlock()

		t.wg.Wait()
		close(t.events)
	})
	return nil
}

func (t *Transport) emit(ev transport.Event) {
	select {
	case t.events <- ev:
	case <-t.stop:
	default:
		t.log.Warn("tcpx: event queue full, dropping", "kind", ev.Kind)
	}
}

// ErrNoSeeds is returned when a TCP transport is asked for with nothing to
// connect to and nothing to listen on.
var ErrNoSeeds = errors.New("tcpx: no listen address")
