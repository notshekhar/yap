package store

import (
	"path/filepath"
	"testing"

	"github.com/notshekhar/yap/internal/identity"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "yap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func key(t *testing.T) identity.PublicKey {
	t.Helper()
	id, err := identity.New()
	if err != nil {
		t.Fatal(err)
	}
	return id.Public()
}

func TestContactRoundTrip(t *testing.T) {
	s := open(t)
	k := key(t)

	if err := s.SaveContact(k, "sam"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Contact(k.NodeID().String())
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("contact was not saved")
	}
	if got.Name != "sam" {
		t.Fatalf("name is %q", got.Name)
	}
	if got.Key != k {
		t.Fatal("the stored key is not the one saved")
	}
	// The address must be reconstructible from storage, since it is the only
	// way to re-share a contact.
	if got.Address != k.Address() {
		t.Fatal("stored contact does not round-trip to the same address")
	}
}

// A peer that announces itself anonymously must not be able to blank out the
// name you chose for them.
func TestAnonymousAnnounceDoesNotEraseAName(t *testing.T) {
	s := open(t)
	k := key(t)

	s.SaveContact(k, "sam")
	s.SaveContact(k, "")

	got, _ := s.Contact(k.NodeID().String())
	if got.Name != "sam" {
		t.Fatalf("name became %q", got.Name)
	}
}

func TestMissingContactIsNotAnError(t *testing.T) {
	s := open(t)
	got, err := s.Contact("nobody")
	if err != nil {
		t.Fatalf("looking up an unknown contact errored: %v", err)
	}
	if got != nil {
		t.Fatal("an unknown contact returned a row")
	}
}

// The mesh delivers duplicates by design: a flood arrives by two paths, or a
// sender retries something that did in fact land. Storing twice must not
// produce two messages or two unread counts.
func TestDuplicateMessageIsIgnored(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "sam")

	msg := &Message{
		ID: "m1", ChatID: "chat1", Author: "chat1",
		Kind: KindText, Body: "hello", CreatedAt: 1000, ReceivedAt: 1000,
		State: StateDelivered,
	}

	fresh, err := s.AddMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Fatal("first insert reported as duplicate")
	}

	fresh, err = s.AddMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("second insert reported as new")
	}

	msgs, _ := s.Messages("chat1", 100, 0)
	if len(msgs) != 1 {
		t.Fatalf("stored %d copies of one message", len(msgs))
	}

	chats, _ := s.Chats()
	if chats[0].Unread != 1 {
		t.Fatalf("unread counted %d times", chats[0].Unread)
	}
}

func TestOwnMessagesAreNotUnread(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "sam")

	s.AddMessage(&Message{
		ID: "m1", ChatID: "chat1", Mine: true,
		Kind: KindText, Body: "hi", CreatedAt: 1000, ReceivedAt: 1000, State: StateSent,
	})

	chats, _ := s.Chats()
	if chats[0].Unread != 0 {
		t.Fatalf("a message I sent counted as unread (%d)", chats[0].Unread)
	}
}

// Acknowledgements arrive out of order on a mesh. A late "sent" must never
// pull a message back from "read".
func TestDeliveryStateNeverGoesBackwards(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")
	s.AddMessage(&Message{
		ID: "m1", ChatID: "chat1", Mine: true,
		Kind: KindText, Body: "hi", CreatedAt: 1, ReceivedAt: 1, State: StateQueued,
	})

	for _, st := range []string{StateSent, StateDelivered, StateRead} {
		if err := s.SetState("m1", st); err != nil {
			t.Fatal(err)
		}
	}

	// Everything that arrives late must be ignored.
	for _, late := range []string{StateSent, StateDelivered, StateQueued} {
		if err := s.SetState("m1", late); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Message("m1")
		if got.State != StateRead {
			t.Fatalf("a late %q downgraded the message to %q", late, got.State)
		}
	}
}

// Failure is set deliberately by the sender's own retry logic, so it is the one
// transition allowed to move against the ranking.
func TestFailureCanOverride(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")
	s.AddMessage(&Message{
		ID: "m1", ChatID: "chat1", Mine: true,
		Kind: KindText, CreatedAt: 1, ReceivedAt: 1, State: StateSent,
	})

	if err := s.SetState("m1", StateFailed); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Message("m1")
	if got.State != StateFailed {
		t.Fatalf("state is %q", got.State)
	}
}

func TestUnknownStateRejected(t *testing.T) {
	s := open(t)
	if err := s.SetState("m1", "teleported"); err == nil {
		t.Fatal("an unknown delivery state was accepted")
	}
}

func TestMessagesReturnedOldestFirst(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")
	for i, ts := range []int64{300, 100, 200} {
		s.AddMessage(&Message{
			ID: string(rune('a' + i)), ChatID: "chat1",
			Kind: KindText, CreatedAt: ts, ReceivedAt: ts, State: StateDelivered,
		})
	}

	msgs, err := s.Messages("chat1", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages", len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i-1].CreatedAt > msgs[i].CreatedAt {
			t.Fatal("messages are not in reading order")
		}
	}
}

// The page limit must take the newest messages, not the oldest, or opening a
// long conversation shows its beginning.
func TestPagingTakesTheNewest(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")
	for i := 0; i < 20; i++ {
		s.AddMessage(&Message{
			ID: string(rune('a' + i)), ChatID: "chat1",
			Kind: KindText, CreatedAt: int64(i * 10), ReceivedAt: 1, State: StateDelivered,
		})
	}

	msgs, _ := s.Messages("chat1", 5, 0)
	if len(msgs) != 5 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[len(msgs)-1].CreatedAt != 190 {
		t.Fatalf("page ends at %d, expected the newest", msgs[len(msgs)-1].CreatedAt)
	}
}

func TestMarkChatReadReportsWhatChanged(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")

	// Two from them, one from me. Only theirs can become read.
	s.AddMessage(&Message{ID: "t1", ChatID: "chat1", Kind: KindText, CreatedAt: 1, ReceivedAt: 1, State: StateDelivered})
	s.AddMessage(&Message{ID: "t2", ChatID: "chat1", Kind: KindText, CreatedAt: 2, ReceivedAt: 2, State: StateDelivered})
	s.AddMessage(&Message{ID: "mine", ChatID: "chat1", Mine: true, Kind: KindText, CreatedAt: 3, ReceivedAt: 3, State: StateSent})

	ids, err := s.MarkChatRead("chat1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("reported %d newly-read messages, want 2", len(ids))
	}

	chats, _ := s.Chats()
	if chats[0].Unread != 0 {
		t.Fatal("unread count was not cleared")
	}

	// Marking again reports nothing, so no receipt is sent twice.
	ids, _ = s.MarkChatRead("chat1")
	if len(ids) != 0 {
		t.Fatalf("re-reading reported %d messages", len(ids))
	}
}

// Deleting removes the content locally. It cannot unsend, and the row stays so
// the conversation keeps its shape.
func TestDeleteClearsContentButKeepsTheRow(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")
	s.AddMessage(&Message{
		ID: "m1", ChatID: "chat1", Kind: KindText, Body: "regrettable",
		BlobID: "b1", CreatedAt: 1, ReceivedAt: 1, State: StateDelivered,
	})

	if err := s.DeleteMessage("m1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Message("m1")
	if got == nil {
		t.Fatal("the row was removed entirely")
	}
	if !got.Deleted || got.Body != "" || got.BlobID != "" {
		t.Fatalf("content survived deletion: %+v", got)
	}
}

func TestUndeliveredSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yap.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.EnsureChat("chat1", "direct", "")
	s.AddMessage(&Message{ID: "q1", ChatID: "chat1", Mine: true, Kind: KindText, CreatedAt: 1, ReceivedAt: 1, State: StateQueued})
	s.AddMessage(&Message{ID: "d1", ChatID: "chat1", Mine: true, Kind: KindText, CreatedAt: 2, ReceivedAt: 2, State: StateDelivered})
	s.Close()

	// Reopen, as a restart would.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	pending, err := s2.Undelivered()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "q1" {
		t.Fatalf("undelivered set is %v", pending)
	}
}

func TestChatPreviewFollowsTheLastMessage(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "sam")

	s.AddMessage(&Message{ID: "m1", ChatID: "chat1", Kind: KindText, Body: "first", CreatedAt: 100, ReceivedAt: 1, State: StateDelivered})
	s.AddMessage(&Message{ID: "m2", ChatID: "chat1", Kind: KindText, Body: "second", CreatedAt: 200, ReceivedAt: 1, State: StateDelivered})

	chats, _ := s.Chats()
	if chats[0].Preview != "second" {
		t.Fatalf("preview is %q", chats[0].Preview)
	}

	// An attachment has no text, so the preview names the kind instead of
	// showing an empty row.
	s.AddMessage(&Message{ID: "m3", ChatID: "chat1", Kind: KindImage, BlobID: "b", CreatedAt: 300, ReceivedAt: 1, State: StateDelivered})
	chats, _ = s.Chats()
	if chats[0].Preview != "[image]" {
		t.Fatalf("attachment preview is %q", chats[0].Preview)
	}
}

func TestPinnedChatsSortFirst(t *testing.T) {
	s := open(t)
	s.EnsureChat("old", "direct", "old")
	s.EnsureChat("new", "direct", "new")
	s.AddMessage(&Message{ID: "a", ChatID: "old", Kind: KindText, CreatedAt: 100, ReceivedAt: 1, State: StateDelivered})
	s.AddMessage(&Message{ID: "b", ChatID: "new", Kind: KindText, CreatedAt: 900, ReceivedAt: 1, State: StateDelivered})

	chats, _ := s.Chats()
	if chats[0].ID != "new" {
		t.Fatal("chats are not ordered by recency")
	}

	if err := s.SetChatFlag("old", "pinned", true); err != nil {
		t.Fatal(err)
	}
	chats, _ = s.Chats()
	if chats[0].ID != "old" {
		t.Fatal("a pinned chat did not sort first")
	}
}

// The flag name reaches SQL, so anything not on the allow list must be refused
// rather than interpolated.
func TestUnknownChatFlagRejected(t *testing.T) {
	s := open(t)
	s.EnsureChat("chat1", "direct", "")
	if err := s.SetChatFlag("chat1", "pinned = 1; DROP TABLE chats; --", true); err == nil {
		t.Fatal("an arbitrary column name was accepted")
	}
}

func TestBlobRoundTrip(t *testing.T) {
	s := open(t)
	data := []byte{0, 1, 2, 250, 255}

	if err := s.PutBlob("b1", "image/jpeg", "photo.jpg", data); err != nil {
		t.Fatal(err)
	}
	mime, name, got, err := s.Blob("b1")
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" || name != "photo.jpg" || string(got) != string(data) {
		t.Fatalf("blob came back as %q %q %v", mime, name, got)
	}

	_, _, missing, err := s.Blob("nope")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatal("an unknown blob returned data")
	}
}

func TestSettings(t *testing.T) {
	s := open(t)
	if got := s.Setting("profile.name", "fallback"); got != "fallback" {
		t.Fatalf("unset setting returned %q", got)
	}
	if err := s.SetSetting("profile.name", "sam"); err != nil {
		t.Fatal(err)
	}
	if got := s.Setting("profile.name", "fallback"); got != "sam" {
		t.Fatalf("setting returned %q", got)
	}
	// Writing again replaces rather than duplicating.
	s.SetSetting("profile.name", "alex")
	if got := s.Setting("profile.name", ""); got != "alex" {
		t.Fatalf("setting was not updated: %q", got)
	}
}

func TestBlockedContact(t *testing.T) {
	s := open(t)
	k := key(t)
	s.SaveContact(k, "nuisance")

	if err := s.SetBlocked(k.NodeID().String(), true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Contact(k.NodeID().String())
	if !got.Blocked {
		t.Fatal("contact was not blocked")
	}
}
