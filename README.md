# yap

Chat over Bluetooth. No server, no internet, no account.

Your address is your public key. Messages are encrypted end to end and travel
to people near you — and onward through them to people near *them*. The devices
that relay a message cannot read it.

```
go build -o yap ./cmd/yap && ./yap
```

It opens <http://127.0.0.1:7474>.

## How it works

Every node is both halves of Bluetooth Low Energy at once: it advertises a
service so strangers can find it, and scans for that service so it can find
them. Neither role alone forms a mesh — two scanners never notice each other.

A message is broadcast to every neighbour, each neighbour rebroadcasts it once,
and a hop count stops it. There is no routing protocol, because the topology
changes every time somebody stands up. What makes the flood affordable is a
dedupe set — each node relays a given message exactly once — and a shortcut for
peers we can already hear directly, so two people in one room do not involve the
rest of the building.

If nobody can reach the recipient right now, the message waits. Your own outbox
retries it, and neighbours will carry it for you for a while, so a message can
cross a building where the two ends are never present at the same time.

## Identity and encryption

An address carries your entire X25519 public key rather than a hash of it:

```
yap:06qxr-c05hn-bar53-5c09a-n2bgj-byxc5-0ww79-srnjk-rk747-2tx92-2j2qw-v
```

That costs 56 characters and buys two things. Nobody can grind a second keypair
onto your address, the way they could against a truncated fingerprint. And
knowing an address means knowing the peer's static key, so the first message can
be encrypted immediately — a **Noise IK** handshake rather than the three-message
XX. On a mesh where the recipient may be two relays away and asleep, a pattern
that needs a round trip before any content moves is close to useless.

Sessions are forward secret (X25519 / ChaCha20-Poly1305 / SHA-256). Every sealed
message carries an explicit nonce, because Noise's implicit counters assume an
ordered reliable channel and Bluetooth is neither: one dropped frame would
desync a pair permanently. A 64-message sliding window closes the replay hole
that explicit nonces open.

The address is also the recovery story, and there isn't one. The key file in
`~/.yap` **is** the account. Lose it and that address is gone for good.

## What a relay learns

That some node sent something to some other node, and how big it was. Routing
headers carry an 8-byte node id, not the full key. Everything else — the text,
the attachments, the receipts, even whether you are typing — is inside the
encryption.

## Storage

Everything is local, in SQLite at `~/.yap/yap.db`. There is no server copy and
no backup: your history lives on the machine that received it. Two of your own
devices each hold their own half of the truth, which is the honest consequence
of having no infrastructure.

## Running two nodes

Bluetooth cannot loop back to the same machine — a Mac never sees its own
advertisement — so two nodes on one laptop need the TCP transport:

```
yap -tcp :7500                        -dir ./a -port 7481 -name alice
yap -tcp :7501 -peer 127.0.0.1:7500   -dir ./b -port 7482 -name bob
```

Same mesh, same handshake, same encryption; only the link layer differs. This is
also how you run yap over Wi-Fi between two machines that cannot pair.

## Flags

```
-name      display name announced to people nearby
-address   print this node's address and exit
-dir       where identity and messages are kept (default ~/.yap)
-port      port for the local web interface
-tcp       carry the mesh over TCP instead of Bluetooth
-peer      comma-separated TCP peers to connect to
-no-radio  run with no transport at all
-v         log what the radio is doing
```

## Platforms

| | mesh | radio |
|---|---|---|
| macOS | yes | Bluetooth LE |
| Linux | yes | not yet — BlueZ supports both roles, so this is a port not a rewrite |
| Windows | yes | not yet |

Everywhere without a radio, yap still runs and the TCP transport still works.

## The macOS note

`tinygo.org/x/bluetooth` documents macOS as central-only. That is true of that
package, not of the OS: `CBPeripheralManager` works, and `tinygo-org/cbgo` binds
it. yap uses cbgo directly to get both roles at once — measured working on
macOS 26.5, advertising and scanning simultaneously.

One rule governs that code: **never call CoreBluetooth from inside a
CoreBluetooth callback.** Doing so segfaults the process with no Go panic, no
stack trace, and a crash report pointing at an idle thread. Delegates here only
hand closures to a driver goroutine.
