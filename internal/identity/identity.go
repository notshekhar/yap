// Package identity is who you are on the mesh.
//
// A yap address carries your entire X25519 public key, not a hash of it. That
// costs 56 characters instead of 16, and buys two things that matter more than
// brevity:
//
//   - Nobody can grind a second keypair onto your address. A truncated
//     fingerprint is only as strong as its length; a 64-bit one falls to a
//     determined attacker with 2^64 work, and the whole point of having
//     identity here is that it cannot be forged.
//   - Knowing an address means knowing the peer's static key, so we can open a
//     Noise IK session in one message instead of the three XX needs. On a mesh
//     where a peer may be two relays away and asleep, one-shot beats round
//     trips.
//
// Routing headers do not carry the full key. They carry NodeID, the first 8
// bytes of its hash, because that field rides on every packet and BLE frames
// are small. Colliding a NodeID misroutes a packet; it never decrypts one.
package identity

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Scheme prefixes every printed address.
	Scheme = "yap"

	// KeySize is the size of an X25519 public or private key.
	KeySize = 32

	// NodeIDSize is the routing identifier: 8 bytes of hashed public key.
	NodeIDSize = 8

	// addrVersion lets a future format change be rejected cleanly rather than
	// decoded into nonsense.
	addrVersion = 1

	// checksumSize guards against typo'd and truncated addresses.
	checksumSize = 2
)

// crockford is base32 without I, L, O or U, so an address can be read aloud
// and typed back without the classic 1/l and 0/O confusions.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NodeID is the short routing handle for a peer.
type NodeID [NodeIDSize]byte

func (n NodeID) String() string { return hex.EncodeToString(n[:]) }

// IsZero reports whether the ID is the zero value, which the wire format uses
// to mean "broadcast".
func (n NodeID) IsZero() bool {
	return n == NodeID{}
}

// ParseNodeID reads the hex form produced by NodeID.String.
func ParseNodeID(s string) (NodeID, error) {
	var n NodeID
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return n, fmt.Errorf("node id: %w", err)
	}
	if len(b) != NodeIDSize {
		return n, fmt.Errorf("node id: want %d bytes, got %d", NodeIDSize, len(b))
	}
	copy(n[:], b)
	return n, nil
}

// PublicKey is a peer's X25519 static public key. It is the peer's identity;
// everything else about them is claimed rather than proven.
type PublicKey [KeySize]byte

// NodeID derives the routing handle from the key.
func (p PublicKey) NodeID() NodeID {
	sum := sha256.Sum256(p[:])
	var n NodeID
	copy(n[:], sum[:NodeIDSize])
	return n
}

// Address renders the printable address: scheme, colon, then version, key and
// checksum in Crockford base32, hyphenated into readable groups.
func (p PublicKey) Address() string {
	body := make([]byte, 0, 1+KeySize+checksumSize)
	body = append(body, addrVersion)
	body = append(body, p[:]...)
	body = append(body, checksum(body)...)
	return Scheme + ":" + group(strings.ToLower(crockford.EncodeToString(body)))
}

// Short is the abbreviated form for dense UI, e.g. "7f3k9…p3n6q". Never use it
// to identify a peer in storage or on the wire; it is decoration.
func (p PublicKey) Short() string {
	raw := strings.ReplaceAll(strings.TrimPrefix(p.Address(), Scheme+":"), "-", "")
	if len(raw) < 12 {
		return raw
	}
	return raw[:5] + "…" + raw[len(raw)-5:]
}

// ECDH converts to the standard library key type for handshake use.
func (p PublicKey) ECDH() (*ecdh.PublicKey, error) {
	return ecdh.X25519().NewPublicKey(p[:])
}

// ParseAddress decodes a printed address back to a public key. It accepts any
// mix of case, hyphens and surrounding space, and the scheme prefix is
// optional so a pasted bare address still works.
func ParseAddress(s string) (PublicKey, error) {
	var pk PublicKey

	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), Scheme+":")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t', '\n':
			return -1
		// Crockford treats these as their digit lookalikes rather than
		// rejecting them, which is the whole reason to use it.
		case 'i', 'l':
			return '1'
		case 'o':
			return '0'
		}
		return r
	}, s)

	body, err := crockford.DecodeString(strings.ToUpper(s))
	if err != nil {
		return pk, fmt.Errorf("address is not valid base32: %w", err)
	}
	if len(body) != 1+KeySize+checksumSize {
		return pk, fmt.Errorf("address is %d bytes, want %d", len(body), 1+KeySize+checksumSize)
	}
	if body[0] != addrVersion {
		return pk, fmt.Errorf("address version %d is not supported", body[0])
	}

	payload, want := body[:1+KeySize], body[1+KeySize:]
	if subtle.ConstantTimeCompare(checksum(payload), want) != 1 {
		return pk, errors.New("address checksum does not match: it was mistyped or truncated")
	}

	copy(pk[:], payload[1:])
	// An all-zero key is a valid X25519 encoding but a degenerate one, and it
	// is what an uninitialised buffer looks like. Refuse it.
	if (pk == PublicKey{}) {
		return pk, errors.New("address encodes an all-zero key")
	}
	return pk, nil
}

// Identity is this node's keypair.
type Identity struct {
	priv *ecdh.PrivateKey
	pub  PublicKey
}

// New generates a fresh identity.
func New() (*Identity, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	return fromPrivate(priv)
}

func fromPrivate(priv *ecdh.PrivateKey) (*Identity, error) {
	var pub PublicKey
	raw := priv.PublicKey().Bytes()
	if len(raw) != KeySize {
		return nil, fmt.Errorf("unexpected public key size %d", len(raw))
	}
	copy(pub[:], raw)
	return &Identity{priv: priv, pub: pub}, nil
}

// FromSeed builds an identity from raw private key bytes.
func FromSeed(seed []byte) (*Identity, error) {
	priv, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	return fromPrivate(priv)
}

// Public returns the public half.
func (id *Identity) Public() PublicKey { return id.pub }

// NodeID returns this node's routing handle.
func (id *Identity) NodeID() NodeID { return id.pub.NodeID() }

// Address returns this node's printable address.
func (id *Identity) Address() string { return id.pub.Address() }

// PrivateBytes exposes the private key for the handshake layer. It never
// leaves the process.
func (id *Identity) PrivateBytes() []byte { return id.priv.Bytes() }

// Load reads the identity at path, generating and saving one if absent. The
// key file is written 0600 and its directory 0700: this file is the account,
// and losing it means losing the address for good.
func Load(path string) (*Identity, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		seed, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil {
			return nil, fmt.Errorf("identity file %s is corrupt: %w", path, decErr)
		}
		return FromSeed(seed)

	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create identity dir: %w", err)
		}
		id, err := New()
		if err != nil {
			return nil, err
		}
		enc := []byte(hex.EncodeToString(id.PrivateBytes()) + "\n")
		if err := os.WriteFile(path, enc, 0o600); err != nil {
			return nil, fmt.Errorf("write identity: %w", err)
		}
		return id, nil

	default:
		return nil, fmt.Errorf("read identity: %w", err)
	}
}

// checksum binds the version and key together so that altering either one
// invalidates the address.
func checksum(payload []byte) []byte {
	h := sha256.New()
	h.Write([]byte("yap-address-v1"))
	h.Write(payload)
	return h.Sum(nil)[:checksumSize]
}

// group hyphenates every 5 characters, which is short enough to read back over
// a desk without losing your place.
func group(s string) string {
	const n = 5
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%n == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}
