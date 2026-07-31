# Working on yap

Read this before changing anything. Most of it is things that were measured
rather than assumed, and a few of them cost real time to find.

## Layout

```
cmd/yap                     CLI and process wiring
internal/identity           keypair, address encoding, node ids
internal/wire               packet format, fragmentation, dedupe
internal/session            Noise IK, sealed messages, replay window
internal/transport          the Transport interface
internal/transport/ble      real radio, macOS only
internal/transport/tcpx     TCP link layer, for dev and Wi-Fi
internal/transport/loopback in-process virtual radio, for tests
internal/mesh               routing: flood, relay, store-and-forward
internal/store              SQLite
internal/app                chat semantics over the mesh
internal/server             HTTP + SSE + embedded web UI
```

The layering is strict downward. The mesh does not know what a message is; the
app does not know what a packet is; the transport does not know what encryption
is. That is what lets the entire product be tested without a radio.

## The rules that are not negotiable

**Never call CoreBluetooth from inside a CoreBluetooth callback.** It segfaults
the process. There is no Go panic, no traceback, and the crash report names an
idle thread, so you will not find it by reading the output. Delegates post
closures to the driver goroutine; only the driver touches CoreBluetooth. This
was measured on macOS 26.5 — `StartAdvertising` from within `DidAddService`
killed it every time, and the identical call from a goroutine worked.

**Two processes on one Mac cannot see each other over BLE.** CoreBluetooth does
not loop a host's own advertisement back to its own scanner. Verified: a
scanner saw 33 other devices and not the peripheral in the next process. Any
end-to-end Bluetooth test needs two machines. Use `-tcp` locally.

**The announce is unauthenticated and must stay untrusted.** It is how strangers
become reachable, so it cannot require a session. It may only ever add a routing
hint and a display name. The key it carries is proven when a handshake with that
key succeeds — never before. `handleAnnounce` checks that the announced key
hashes to the source it arrived from; do not remove that.

**Dedupe before anything else.** `handlePacket` marks the message id first. Skip
it and a room of n mutually visible peers turns one message into O(n²) frames.

## Testing

```
go test -race ./...
```

`internal/transport/loopback` is a virtual radio where the test decides the
topology. It can do things the real one cannot: put nodes in a line so traffic
must be relayed, drop a share of packets, partition the room. Use it. Its RNG is
seeded fixed on purpose — a mesh test that cannot be reproduced is worse than no
test.

To exercise the whole product without Bluetooth:

```
go build -o /tmp/yap ./cmd/yap
/tmp/yap -tcp :7500                      -dir /tmp/a -port 7481 -name alice -open=false
/tmp/yap -tcp :7501 -peer 127.0.0.1:7500 -dir /tmp/b -port 7482 -name bob   -open=false
```

Screenshotting the UI headless: Chrome's `--virtual-time-budget` never expires
while the SSE stream is open, so the screenshot hangs. Omit it, background the
process, and kill it after a fixed sleep. A conversation is deep-linkable at
`#<node-id>`, which is how you screenshot a thread rather than the empty state.

## Things that are the way they are for a reason

**Explicit nonces.** Noise's transport phase assumes an ordered reliable
channel. Bluetooth drops frames when somebody walks behind a wall. With implicit
counters one loss desyncs a pair forever and every later message fails to
decrypt — "chat randomly stops working". So the nonce is on the wire and the
receiver sets it, with a 64-wide replay window to close the hole that opens.

**A forged message must not consume a replay slot.** `Open` records the nonce
only after decryption succeeds. Otherwise anyone can lock out a genuine message
by guessing its nonce.

**Delivery state never goes backwards.** Acknowledgements arrive out of order; a
late "sent" must not pull a message back from "read". `SetState` ranks them.
Failure is the one exception and is set deliberately.

**`AddMessage` is idempotent and reports it.** The mesh delivers duplicates by
design. The boolean is what stops the UI announcing the same message twice.

**`MaxFragments` bounds pieces, not bytes**, so what it permits depends on the
link. `MaxMessageBytes` is the actual memory bound; fragment counting alone does
not bound anything when the MTU is large.

**Attachments are capped at 96 KB and the browser downscales to fit.** Bluetooth
moves a few kilobytes per second. A megabyte photo holds the radio for minutes,
starves every other conversation on it, and usually fails partway.

## UI

The skin is hehe's, deliberately: its oklch tokens, the mint accent, Geist, and
`border-radius: 0` on everything. The shape is WhatsApp's. What is yap's own is
the vocabulary for a network you cannot see — the range meter, the transit
ticks, and the airspace gauges.

`[hidden] { display: none !important }` is load-bearing. The UA rule for
`[hidden]` loses to any author `display`, so a class setting `display:flex`
silently un-hides things. hehe shipped that bug once.

## Still open

- Real BLE between two machines is unverified. Everything below the radio is
  tested; the radio itself has only been proven to advertise without crashing.
- Group chats. The store has a `kind` column for it and nothing else exists.
- Linux radio, via BlueZ, which supports both roles natively.
- No test covers `internal/app` or `internal/server` directly.
