package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddressRoundTrip(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	addr := id.Address()
	if !strings.HasPrefix(addr, "yap:") {
		t.Fatalf("address %q lacks scheme", addr)
	}

	got, err := ParseAddress(addr)
	if err != nil {
		t.Fatalf("parse own address: %v", err)
	}
	if got != id.Public() {
		t.Fatal("round trip did not recover the public key")
	}
}

func TestAddressIsForgivingAboutFormatting(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	addr := id.Address()
	bare := strings.TrimPrefix(addr, "yap:")

	variants := map[string]string{
		"canonical":     addr,
		"no scheme":     bare,
		"no hyphens":    strings.ReplaceAll(addr, "-", ""),
		"upper case":    strings.ToUpper(addr),
		"padded spaces": "  " + addr + "\n",
	}
	for name, v := range variants {
		got, err := ParseAddress(v)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != id.Public() {
			t.Errorf("%s: wrong key recovered", name)
		}
	}
}

// Crockford's whole reason for existing: a human reading an address aloud
// cannot distinguish these, so the parser must not either.
func TestAddressToleratesLookalikeCharacters(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(id.Address(), "yap:")

	confused := strings.NewReplacer("1", "l", "0", "O").Replace(addr)
	got, err := ParseAddress(confused)
	if err != nil {
		t.Fatalf("lookalike substitution rejected: %v", err)
	}
	if got != id.Public() {
		t.Fatal("lookalike substitution recovered the wrong key")
	}
}

func TestChecksumCatchesTypos(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	addr := []byte(strings.ReplaceAll(strings.TrimPrefix(id.Address(), "yap:"), "-", ""))

	// Flip one character to something else in the alphabet at every position
	// and require that not one of them decodes to a usable key.
	for i := range addr {
		orig := addr[i]
		repl := byte('2')
		if orig == repl {
			repl = '3'
		}
		mutated := make([]byte, len(addr))
		copy(mutated, addr)
		mutated[i] = repl

		if got, err := ParseAddress(string(mutated)); err == nil && got == id.Public() {
			t.Fatalf("typo at index %d went undetected", i)
		}
	}
}

func TestTruncatedAddressRejected(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.ReplaceAll(strings.TrimPrefix(id.Address(), "yap:"), "-", "")
	if _, err := ParseAddress(addr[:len(addr)-4]); err == nil {
		t.Fatal("truncated address accepted")
	}
}

func TestNodeIDDerivation(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	n := id.NodeID()
	if n.IsZero() {
		t.Fatal("node id is zero")
	}
	if n != id.Public().NodeID() {
		t.Fatal("node id is not stable")
	}

	parsed, err := ParseNodeID(n.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != n {
		t.Fatal("node id did not survive hex round trip")
	}
}

func TestDistinctIdentitiesDiffer(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if a.Public() == b.Public() {
		t.Fatal("two generated identities share a key")
	}
	if a.NodeID() == b.NodeID() {
		t.Fatal("two generated identities share a node id")
	}
}

func TestLoadCreatesThenReusesIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "identity")

	first, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Public() != second.Public() {
		t.Fatal("identity was not stable across loads")
	}

	// The key file is the account. If it is group- or world-readable, anyone
	// with a shell on the box can become you.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("identity file mode is %o, want 600", perm)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(path, []byte("not hex at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupt identity file was accepted")
	}
}

func TestParseAddressRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "yap:", "yap:!!!!", "hello world", strings.Repeat("2", 200)} {
		if _, err := ParseAddress(s); err == nil {
			t.Errorf("accepted garbage address %q", s)
		}
	}
}
