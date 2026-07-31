//go:build darwin

// Package ble is the real radio on macOS.
//
// Every node is both halves of Bluetooth Low Energy at once: a peripheral
// advertising a service so strangers can find it, and a central scanning for
// that same service so it can find them. Neither role alone forms a mesh —
// two centrals cannot talk, and two peripherals never notice each other.
//
// # The rule that governs this whole file
//
// Never call CoreBluetooth from inside a CoreBluetooth callback.
//
// This is not style. Doing it segfaults the process outright, with no Go
// panic, no stack trace, and a crash report that points at an idle thread. It
// was measured on macOS 26.5: calling StartAdvertising from within DidAddService
// killed the process every time, while the same call from an ordinary goroutine
// worked. So delegate methods here do exactly one thing — hand a closure to the
// driver goroutine — and every CoreBluetooth call happens on that goroutine.
//
// # Why the published support matrix says this is impossible
//
// tinygo.org/x/bluetooth documents macOS as central-only, and for that package
// it is accurate. The limitation is in its darwin backend, not in the OS:
// CBPeripheralManager exists and works, and cbgo — the layer tinygo's backend
// is built on — binds it. This package uses cbgo directly to get both roles.
package ble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinygo-org/cbgo"

	"github.com/notshekhar/yap/internal/transport"
	"github.com/notshekhar/yap/internal/wire"
)

// The service and characteristic that make a device a yap node. A single
// characteristic carries both directions: centrals write to it, and the
// peripheral notifies back on it.
const (
	ServiceUUID        = "6ba1de6b-3ab6-4d77-9ea1-cb6422720001"
	CharacteristicUUID = "6ba1de6b-3ab6-4d77-9ea1-cb6422720002"
)

const (
	// fallbackMTU is used when CoreBluetooth reports something implausible.
	// BLE's floor is 20 usable bytes; anything at or below that is a bad read
	// rather than a real link.
	fallbackMTU = 180

	// minMTU is the smallest we will attempt to fragment for.
	minMTU = 20

	// rescanInterval re-arms scanning. macOS quietly stops delivering
	// discoveries after a while, and a mesh that stops noticing new people is
	// broken in a way nobody reports as a bug.
	rescanInterval = 30 * time.Second

	// signalBuffer is generous because delegate callbacks arrive on a single
	// serial dispatch queue: if we make CoreBluetooth wait, we stall every
	// other Bluetooth event too.
	signalBuffer = 1024
)

// outLink is a peer our central connected to. We write; they notify.
type outLink struct {
	prph cbgo.Peripheral
	chr  cbgo.Characteristic
	mtu  int

	// ready is false until the characteristic is discovered and subscribed.
	ready bool

	queue    [][]byte
	inFlight bool

	reasm *wire.Reassembler
}

// inLink is a peer whose central subscribed to us. We notify; they write.
type inLink struct {
	central cbgo.Central
	mtu     int
	queue   [][]byte
	reasm   *wire.Reassembler
}

// Transport is a BLE mesh radio.
type Transport struct {
	name string
	log  *slog.Logger

	svcUUID cbgo.UUID
	chrUUID cbgo.UUID

	pm  cbgo.PeripheralManager
	cm  cbgo.CentralManager
	chr cbgo.MutableCharacteristic

	// cmds is the only path to CoreBluetooth. Delegates post closures here and
	// the driver goroutine runs them, one at a time.
	cmds   chan func()
	events chan transport.Event

	// Driver-goroutine-owned. No locking: only the driver touches these.
	outbound      map[string]*outLink
	inbound       map[string]*inLink
	connecting    map[string]bool
	advertising   bool
	notifyBlocked bool

	// Shared with callers of Send/Links, hence the mutex.
	mu    sync.Mutex
	links map[transport.LinkID]int

	// fragSeq labels fragment groups. Send is called from the mesh goroutine,
	// so this counter is touched off-driver and needs to be atomic.
	fragSeq atomic.Uint32

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	started  bool
}

var _ transport.Transport = (*Transport)(nil)

// New creates a BLE transport. name is advertised so a human scanning nearby
// devices sees something recognisable; it carries no authority.
func New(name string, log *slog.Logger) (*Transport, error) {
	svc, err := cbgo.ParseUUID(ServiceUUID)
	if err != nil {
		return nil, fmt.Errorf("service uuid: %w", err)
	}
	chr, err := cbgo.ParseUUID(CharacteristicUUID)
	if err != nil {
		return nil, fmt.Errorf("characteristic uuid: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}

	return &Transport{
		name:       advertisedName(name),
		log:        log,
		svcUUID:    svc,
		chrUUID:    chr,
		cmds:       make(chan func(), signalBuffer),
		events:     make(chan transport.Event, 512),
		outbound:   make(map[string]*outLink),
		inbound:    make(map[string]*inLink),
		connecting: make(map[string]bool),
		links:      make(map[transport.LinkID]int),
		stop:       make(chan struct{}),
	}, nil
}

// advertisedName keeps the local name short. A BLE advertisement is 31 bytes
// and a 128-bit service UUID eats 18 of them, so a long name simply will not
// fit and the whole advertisement silently fails to include it.
func advertisedName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "yap"
	}
	if len(name) > 8 {
		name = name[:8]
	}
	return name
}

// Start powers up both roles.
func (t *Transport) Start(ctx context.Context) error {
	if t.started {
		return errors.New("ble: already started")
	}
	t.started = true

	t.wg.Add(1)
	go t.driver()

	t.pm = cbgo.NewPeripheralManager(nil)
	t.pm.SetDelegate(&peripheralDelegate{t: t})

	t.cm = cbgo.NewCentralManager(nil)
	t.cm.SetDelegate(&centralDelegate{t: t})

	// Both managers report their state asynchronously; the delegates start the
	// real work when powered on. Nothing to wait for here.
	return nil
}

// driver is the only goroutine that talks to CoreBluetooth.
func (t *Transport) driver() {
	defer t.wg.Done()

	rescan := time.NewTicker(rescanInterval)
	defer rescan.Stop()

	for {
		select {
		case <-t.stop:
			return
		case fn := <-t.cmds:
			fn()
		case <-rescan.C:
			t.restartScan()
		}
	}
}

// post hands work to the driver goroutine. Called from delegate callbacks, so
// it must never call CoreBluetooth itself.
func (t *Transport) post(fn func()) {
	select {
	case t.cmds <- fn:
	case <-t.stop:
	}
}

// Send queues a packet for one link.
func (t *Transport) Send(link transport.LinkID, packet []byte) error {
	t.mu.Lock()
	mtu, ok := t.links[link]
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", transport.ErrNoSuchLink, link)
	}

	frames, err := wire.Split(packet, mtu, t.fragSeq.Add(1))
	if err != nil {
		return err
	}

	cp := make([][]byte, len(frames))
	copy(cp, frames)

	t.post(func() { t.enqueue(link, cp) })
	return nil
}

// Broadcast queues a packet for every link, continuing past failures so that
// one bad peer cannot stop the flood.
func (t *Transport) Broadcast(packet []byte) error {
	t.mu.Lock()
	links := make([]transport.LinkID, 0, len(t.links))
	for l := range t.links {
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
	out := make([]transport.Link, 0, len(t.links))
	for id, mtu := range t.links {
		out = append(out, transport.Link{ID: id, MTU: mtu, Addr: string(id)})
	}
	return out
}

// Events returns the event stream.
func (t *Transport) Events() <-chan transport.Event { return t.events }

// Close releases the radio.
func (t *Transport) Close() error {
	t.stopOnce.Do(func() {
		// Stop the radio before tearing down the driver, and do it from here
		// rather than from a callback.
		if t.started {
			t.pm.StopAdvertising()
			t.cm.StopScan()
		}
		close(t.stop)
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
		t.log.Warn("ble: event queue full, dropping", "kind", ev.Kind)
	}
}

// linkUp records a link and tells the mesh about it.
func (t *Transport) linkUp(id transport.LinkID, mtu int) {
	t.mu.Lock()
	t.links[id] = mtu
	t.mu.Unlock()
	t.emit(transport.Event{Kind: transport.LinkUp, Link: id, MTU: mtu})
}

func (t *Transport) linkDown(id transport.LinkID) {
	t.mu.Lock()
	_, existed := t.links[id]
	delete(t.links, id)
	t.mu.Unlock()
	if existed {
		t.emit(transport.Event{Kind: transport.LinkDown, Link: id})
	}
}

// Link ids are prefixed by role, because a peripheral's identifier and a
// central's identifier come from different namespaces and can collide.
func outID(u cbgo.UUID) transport.LinkID { return transport.LinkID("out:" + u.String()) }
func inID(u cbgo.UUID) transport.LinkID  { return transport.LinkID("in:" + u.String()) }

// ---------------------------------------------------------------------------
// Driver-goroutine work
// ---------------------------------------------------------------------------

func (t *Transport) enqueue(link transport.LinkID, frames [][]byte) {
	key := strings.TrimPrefix(string(link), "out:")
	if o, ok := t.outbound[key]; ok && strings.HasPrefix(string(link), "out:") {
		o.queue = append(o.queue, frames...)
		t.pumpOut(key, o)
		return
	}

	key = strings.TrimPrefix(string(link), "in:")
	if i, ok := t.inbound[key]; ok && strings.HasPrefix(string(link), "in:") {
		i.queue = append(i.queue, frames...)
		t.pumpIn(key, i)
		return
	}
}

// pumpOut writes queued frames to a peer we connected to.
//
// Writes go out one at a time, each waiting for its completion callback.
// CoreBluetooth will accept a burst and then silently drop the excess, which
// on a mesh looks like messages that arrive with holes in them.
func (t *Transport) pumpOut(key string, o *outLink) {
	if !o.ready || o.inFlight || len(o.queue) == 0 {
		return
	}
	frame := o.queue[0]
	o.queue = o.queue[1:]
	o.inFlight = true
	o.prph.WriteCharacteristic(frame, o.chr, true)
}

// pumpIn notifies queued frames to a peer subscribed to us.
//
// UpdateValue returns false when CoreBluetooth's transmit queue is full. The
// frame must then be put back and retried when IsReadyToUpdateSubscribers
// fires; dropping it instead is the classic source of truncated transfers.
func (t *Transport) pumpIn(key string, i *inLink) {
	if t.notifyBlocked {
		return
	}
	for len(i.queue) > 0 {
		frame := i.queue[0]
		if !t.pm.UpdateValue(frame, t.chr.Characteristic(), []cbgo.Central{i.central}) {
			t.notifyBlocked = true
			return
		}
		i.queue = i.queue[1:]
	}
}

// resumeNotifies is called when CoreBluetooth has room again.
func (t *Transport) resumeNotifies() {
	t.notifyBlocked = false
	for key, i := range t.inbound {
		t.pumpIn(key, i)
		if t.notifyBlocked {
			return
		}
	}
}

func (t *Transport) startAdvertising() {
	if t.advertising {
		return
	}
	t.advertising = true
	t.pm.StartAdvertising(cbgo.AdvData{
		LocalName:    t.name,
		ServiceUUIDs: []cbgo.UUID{t.svcUUID},
	})
}

func (t *Transport) restartScan() {
	if t.cm.State() != cbgo.ManagerStatePoweredOn {
		return
	}
	// Filtering by service UUID is what keeps us from connecting to every
	// fitness tracker in the building.
	t.cm.Scan([]cbgo.UUID{t.svcUUID}, &cbgo.CentralManagerScanOpts{AllowDuplicates: false})
}

// receive feeds an inbound frame through reassembly and emits whole packets.
func (t *Transport) receive(id transport.LinkID, reasm *wire.Reassembler, frame []byte) {
	packet, err := reasm.Push(frame)
	if err != nil {
		t.log.Debug("ble: bad frame", "link", id, "err", err)
		return
	}
	if packet == nil {
		return // more fragments still to come
	}
	t.emit(transport.Event{Kind: transport.PacketReceived, Link: id, Packet: packet})
}

func clampMTU(n int) int {
	if n < minMTU {
		return fallbackMTU
	}
	return n
}

// ---------------------------------------------------------------------------
// Peripheral role: we are discoverable, others write to us
// ---------------------------------------------------------------------------

type peripheralDelegate struct {
	cbgo.PeripheralManagerDelegateBase
	t *Transport
}

func (d *peripheralDelegate) PeripheralManagerDidUpdateState(pm cbgo.PeripheralManager) {
	state := pm.State()
	d.t.post(func() {
		if state != cbgo.ManagerStatePoweredOn {
			d.t.log.Info("ble: peripheral not available", "state", state)
			return
		}
		chr := cbgo.NewMutableCharacteristic(
			d.t.chrUUID,
			cbgo.CharacteristicPropertyWrite|
				cbgo.CharacteristicPropertyWriteWithoutResponse|
				cbgo.CharacteristicPropertyNotify,
			nil,
			cbgo.AttributePermissionsReadable|cbgo.AttributePermissionsWriteable,
		)
		svc := cbgo.NewMutableService(d.t.svcUUID, true)
		svc.SetCharacteristics([]cbgo.MutableCharacteristic{chr})

		d.t.chr = chr
		d.t.pm.AddService(svc)
	})
}

func (d *peripheralDelegate) DidAddService(pm cbgo.PeripheralManager, svc cbgo.Service, err error) {
	d.t.post(func() {
		if err != nil {
			d.t.log.Error("ble: could not publish service", "err", err)
			return
		}
		d.t.startAdvertising()
	})
}

func (d *peripheralDelegate) DidStartAdvertising(pm cbgo.PeripheralManager, err error) {
	d.t.post(func() {
		if err != nil {
			d.t.advertising = false
			d.t.log.Error("ble: could not advertise", "err", err)
			return
		}
		d.t.log.Info("ble: advertising", "name", d.t.name)
	})
}

func (d *peripheralDelegate) CentralDidSubscribe(pm cbgo.PeripheralManager, c cbgo.Central, chr cbgo.Characteristic) {
	key := c.Identifier().String()
	mtu := clampMTU(c.MaximumUpdateValueLength())
	d.t.post(func() {
		d.t.inbound[key] = &inLink{central: c, mtu: mtu, reasm: wire.NewReassembler()}
		d.t.log.Info("ble: peer subscribed", "peer", key, "mtu", mtu)
		d.t.linkUp(inID(c.Identifier()), mtu)
	})
}

func (d *peripheralDelegate) CentralDidUnsubscribe(pm cbgo.PeripheralManager, c cbgo.Central, chr cbgo.Characteristic) {
	key := c.Identifier().String()
	id := inID(c.Identifier())
	d.t.post(func() {
		delete(d.t.inbound, key)
		d.t.linkDown(id)
	})
}

func (d *peripheralDelegate) IsReadyToUpdateSubscribers(pm cbgo.PeripheralManager) {
	d.t.post(d.t.resumeNotifies)
}

func (d *peripheralDelegate) DidReceiveWriteRequests(pm cbgo.PeripheralManager, reqs []cbgo.ATTRequest) {
	if len(reqs) == 0 {
		return
	}
	// Copy out of the request objects now: they belong to CoreBluetooth and
	// are not valid once this callback returns.
	type incoming struct {
		key   string
		id    transport.LinkID
		frame []byte
	}
	batch := make([]incoming, 0, len(reqs))
	for _, r := range reqs {
		cent := r.Central()
		val := r.Value()
		frame := make([]byte, len(val))
		copy(frame, val)
		batch = append(batch, incoming{
			key:   cent.Identifier().String(),
			id:    inID(cent.Identifier()),
			frame: frame,
		})
	}
	first := reqs[0]

	d.t.post(func() {
		// Responding is required for writes that expect one, and must happen
		// off the callback like every other CoreBluetooth call.
		d.t.pm.RespondToRequest(first, cbgo.ATTErrorSuccess)

		for _, in := range batch {
			link, ok := d.t.inbound[in.key]
			if !ok {
				// A peer can write before subscribing. Accept it and treat the
				// write itself as the link coming up.
				link = &inLink{mtu: fallbackMTU, reasm: wire.NewReassembler()}
				d.t.inbound[in.key] = link
				d.t.linkUp(in.id, link.mtu)
			}
			d.t.receive(in.id, link.reasm, in.frame)
		}
	})
}

// ---------------------------------------------------------------------------
// Central role: we scan, connect and subscribe
// ---------------------------------------------------------------------------

type centralDelegate struct {
	cbgo.CentralManagerDelegateBase
	t *Transport
}

func (d *centralDelegate) CentralManagerDidUpdateState(cm cbgo.CentralManager) {
	state := cm.State()
	d.t.post(func() {
		if state != cbgo.ManagerStatePoweredOn {
			d.t.log.Info("ble: central not available", "state", state)
			return
		}
		d.t.restartScan()
	})
}

func (d *centralDelegate) DidDiscoverPeripheral(cm cbgo.CentralManager, prph cbgo.Peripheral, af cbgo.AdvFields, rssi int) {
	key := prph.Identifier().String()
	d.t.post(func() {
		if _, ok := d.t.outbound[key]; ok {
			return
		}
		if d.t.connecting[key] {
			return
		}
		d.t.connecting[key] = true
		d.t.log.Info("ble: found a node", "peer", key, "name", af.LocalName, "rssi", rssi)
		d.t.cm.Connect(prph, nil)
	})
}

func (d *centralDelegate) DidConnectPeripheral(cm cbgo.CentralManager, prph cbgo.Peripheral) {
	key := prph.Identifier().String()
	d.t.post(func() {
		delete(d.t.connecting, key)
		d.t.outbound[key] = &outLink{
			prph:  prph,
			mtu:   clampMTU(prph.MaximumWriteValueLength(true)),
			reasm: wire.NewReassembler(),
		}
		prph.SetDelegate(&prphDelegate{t: d.t})
		prph.DiscoverServices([]cbgo.UUID{d.t.svcUUID})
	})
}

func (d *centralDelegate) DidFailToConnectPeripheral(cm cbgo.CentralManager, prph cbgo.Peripheral, err error) {
	key := prph.Identifier().String()
	d.t.post(func() {
		delete(d.t.connecting, key)
		d.t.log.Debug("ble: connect failed", "peer", key, "err", err)
	})
}

func (d *centralDelegate) DidDisconnectPeripheral(cm cbgo.CentralManager, prph cbgo.Peripheral, err error) {
	key := prph.Identifier().String()
	id := outID(prph.Identifier())
	d.t.post(func() {
		// Drop every reference to this peripheral immediately. cbgo does not
		// retain it, so touching it after disconnect risks a use-after-free
		// rather than a clean error.
		delete(d.t.outbound, key)
		delete(d.t.connecting, key)
		d.t.linkDown(id)

		// People walk out of range and come back constantly, so rediscovery
		// has to keep working.
		d.t.restartScan()
	})
}

// prphDelegate handles one connected peripheral's own callbacks.
type prphDelegate struct {
	cbgo.PeripheralDelegateBase
	t *Transport
}

func (d *prphDelegate) DidDiscoverServices(prph cbgo.Peripheral, err error) {
	key := prph.Identifier().String()
	d.t.post(func() {
		if err != nil {
			d.t.log.Debug("ble: service discovery failed", "peer", key, "err", err)
			return
		}
		for _, svc := range prph.Services() {
			if svc.UUID().String() == d.t.svcUUID.String() {
				prph.DiscoverCharacteristics([]cbgo.UUID{d.t.chrUUID}, svc)
				return
			}
		}
	})
}

func (d *prphDelegate) DidDiscoverCharacteristics(prph cbgo.Peripheral, svc cbgo.Service, err error) {
	key := prph.Identifier().String()
	d.t.post(func() {
		if err != nil {
			d.t.log.Debug("ble: characteristic discovery failed", "peer", key, "err", err)
			return
		}
		o, ok := d.t.outbound[key]
		if !ok {
			return
		}
		for _, chr := range svc.Characteristics() {
			if chr.UUID().String() != d.t.chrUUID.String() {
				continue
			}
			o.chr = chr
			o.ready = true
			// Subscribing is what opens the return path: without it we could
			// write to them but never hear an answer.
			prph.SetNotify(true, chr)
			d.t.log.Info("ble: link established", "peer", key, "mtu", o.mtu)
			d.t.linkUp(outID(prph.Identifier()), o.mtu)
			d.t.pumpOut(key, o)
			return
		}
	})
}

func (d *prphDelegate) DidUpdateValueForCharacteristic(prph cbgo.Peripheral, chr cbgo.Characteristic, err error) {
	if err != nil {
		return
	}
	key := prph.Identifier().String()
	id := outID(prph.Identifier())
	val := chr.Value()
	frame := make([]byte, len(val))
	copy(frame, val)

	d.t.post(func() {
		o, ok := d.t.outbound[key]
		if !ok {
			return
		}
		d.t.receive(id, o.reasm, frame)
	})
}

func (d *prphDelegate) DidWriteValueForCharacteristic(prph cbgo.Peripheral, chr cbgo.Characteristic, err error) {
	key := prph.Identifier().String()
	d.t.post(func() {
		o, ok := d.t.outbound[key]
		if !ok {
			return
		}
		o.inFlight = false
		if err != nil {
			d.t.log.Debug("ble: write failed", "peer", key, "err", err)
		}
		d.t.pumpOut(key, o)
	})
}
