// Package store is the local database: contacts, chats, messages and blobs.
//
// Everything here is this device's own record. There is no server copy and no
// backup: a yap history lives on the machine that received it, and if two of
// your devices were both in the room they each hold their own half of the
// truth. That is the honest consequence of having no infrastructure, and the
// UI says so rather than implying a cloud that does not exist.
package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/notshekhar/yap/internal/identity"
)

// Message delivery states, which is what the ticks in the UI are showing.
const (
	// StateQueued means it is written down but has not left this machine.
	StateQueued = "queued"
	// StateSent means it went out on the radio.
	StateSent = "sent"
	// StateDelivered means the recipient's node acknowledged it.
	StateDelivered = "delivered"
	// StateRead means the recipient opened the conversation.
	StateRead = "read"
	// StateFailed means it was abandoned after too many attempts.
	StateFailed = "failed"
)

// Message kinds.
const (
	KindText  = "text"
	KindImage = "image"
	KindFile  = "file"
)

// Store is the local database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// WAL keeps the UI's reads from blocking behind the mesh's writes, which
	// otherwise shows up as the chat list freezing while a message arrives.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS contacts (
    node_id    TEXT PRIMARY KEY,
    pubkey     BLOB NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    added_at   INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL DEFAULT 0,
    blocked    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chats (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL DEFAULT 'direct',
    title      TEXT NOT NULL DEFAULT '',
    last_at    INTEGER NOT NULL DEFAULT 0,
    unread     INTEGER NOT NULL DEFAULT 0,
    pinned     INTEGER NOT NULL DEFAULT 0,
    archived   INTEGER NOT NULL DEFAULT 0,
    muted      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    id          TEXT PRIMARY KEY,
    chat_id     TEXT NOT NULL,
    author      TEXT NOT NULL,
    mine        INTEGER NOT NULL DEFAULT 0,
    kind        TEXT NOT NULL DEFAULT 'text',
    body        TEXT NOT NULL DEFAULT '',
    blob_id     TEXT NOT NULL DEFAULT '',
    reply_to    TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    state       TEXT NOT NULL DEFAULT 'queued',
    deleted     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS messages_by_chat ON messages(chat_id, created_at);

CREATE TABLE IF NOT EXISTS blobs (
    id    TEXT PRIMARY KEY,
    mime  TEXT NOT NULL DEFAULT '',
    name  TEXT NOT NULL DEFAULT '',
    bytes BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

// Contact is somebody you can message.
type Contact struct {
	NodeID   string             `json:"node_id"`
	Key      identity.PublicKey `json:"-"`
	Address  string             `json:"address"`
	Name     string             `json:"name"`
	AddedAt  int64              `json:"added_at"`
	LastSeen int64              `json:"last_seen"`
	Blocked  bool               `json:"blocked"`

	// Online is filled in from the live mesh rather than the database.
	Online bool `json:"online"`
}

// SaveContact adds or updates a contact.
//
// The name is updated only when a non-empty one is supplied, so a peer that
// announces itself anonymously cannot blank out a name you chose for them.
func (s *Store) SaveContact(key identity.PublicKey, name string) error {
	id := key.NodeID().String()
	now := time.Now().UnixMilli()

	_, err := s.db.Exec(`
        INSERT INTO contacts (node_id, pubkey, name, added_at, last_seen)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT(node_id) DO UPDATE SET
            name      = CASE WHEN excluded.name != '' THEN excluded.name ELSE contacts.name END,
            last_seen = excluded.last_seen`,
		id, key[:], name, now, now)
	if err != nil {
		return fmt.Errorf("save contact: %w", err)
	}
	return nil
}

// TouchContact records that a peer was just seen.
func (s *Store) TouchContact(key identity.PublicKey) error {
	_, err := s.db.Exec(`UPDATE contacts SET last_seen = ? WHERE node_id = ?`,
		time.Now().UnixMilli(), key.NodeID().String())
	return err
}

// Contact looks one up.
func (s *Store) Contact(nodeID string) (*Contact, error) {
	row := s.db.QueryRow(`
        SELECT node_id, pubkey, name, added_at, last_seen, blocked
        FROM contacts WHERE node_id = ?`, nodeID)
	return scanContact(row)
}

// Contacts lists everyone, most recently seen first.
func (s *Store) Contacts() ([]*Contact, error) {
	rows, err := s.db.Query(`
        SELECT node_id, pubkey, name, added_at, last_seen, blocked
        FROM contacts ORDER BY last_seen DESC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()

	var out []*Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetBlocked blocks or unblocks a contact.
func (s *Store) SetBlocked(nodeID string, blocked bool) error {
	_, err := s.db.Exec(`UPDATE contacts SET blocked = ? WHERE node_id = ?`, boolInt(blocked), nodeID)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanContact(row scanner) (*Contact, error) {
	var (
		c       Contact
		raw     []byte
		blocked int
	)
	if err := row.Scan(&c.NodeID, &raw, &c.Name, &c.AddedAt, &c.LastSeen, &blocked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read contact: %w", err)
	}
	if len(raw) != identity.KeySize {
		return nil, fmt.Errorf("contact %s has a %d-byte key", c.NodeID, len(raw))
	}
	copy(c.Key[:], raw)
	c.Address = c.Key.Address()
	c.Blocked = blocked != 0
	return &c, nil
}

// ---------------------------------------------------------------------------
// Chats
// ---------------------------------------------------------------------------

// Chat is a conversation.
type Chat struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	LastAt   int64  `json:"last_at"`
	Unread   int    `json:"unread"`
	Pinned   bool   `json:"pinned"`
	Archived bool   `json:"archived"`
	Muted    bool   `json:"muted"`

	// Preview and Online are joined in for the chat list.
	Preview string `json:"preview"`
	Online  bool   `json:"online"`
}

// EnsureChat creates a conversation if it does not exist.
func (s *Store) EnsureChat(id, kind, title string) error {
	_, err := s.db.Exec(`
        INSERT INTO chats (id, kind, title) VALUES (?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            title = CASE WHEN excluded.title != '' THEN excluded.title ELSE chats.title END`,
		id, kind, title)
	if err != nil {
		return fmt.Errorf("ensure chat: %w", err)
	}
	return nil
}

// Chats lists conversations for the sidebar, pinned first then most recent.
func (s *Store) Chats() ([]*Chat, error) {
	rows, err := s.db.Query(`
        SELECT c.id, c.kind, c.title, c.last_at, c.unread, c.pinned, c.archived, c.muted,
               COALESCE((
                   SELECT CASE WHEN m.kind = 'text' THEN m.body ELSE '[' || m.kind || ']' END
                   FROM messages m
                   WHERE m.chat_id = c.id AND m.deleted = 0
                   ORDER BY m.created_at DESC LIMIT 1
               ), '')
        FROM chats c
        ORDER BY c.pinned DESC, c.last_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()

	var out []*Chat
	for rows.Next() {
		var (
			c                       Chat
			pinned, archived, muted int
		)
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.LastAt, &c.Unread,
			&pinned, &archived, &muted, &c.Preview); err != nil {
			return nil, fmt.Errorf("read chat: %w", err)
		}
		c.Pinned, c.Archived, c.Muted = pinned != 0, archived != 0, muted != 0
		out = append(out, &c)
	}
	return out, rows.Err()
}

// SetChatFlag toggles pinned, archived or muted.
func (s *Store) SetChatFlag(id, flag string, on bool) error {
	switch flag {
	case "pinned", "archived", "muted":
	default:
		return fmt.Errorf("unknown chat flag %q", flag)
	}
	// The column name is validated above, never interpolated from raw input.
	_, err := s.db.Exec(`UPDATE chats SET `+flag+` = ? WHERE id = ?`, boolInt(on), id)
	return err
}

// MarkRead clears the unread count.
func (s *Store) MarkRead(chatID string) error {
	_, err := s.db.Exec(`UPDATE chats SET unread = 0 WHERE id = ?`, chatID)
	return err
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

// Message is one entry in a conversation.
type Message struct {
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	Author     string `json:"author"`
	Mine       bool   `json:"mine"`
	Kind       string `json:"kind"`
	Body       string `json:"body"`
	BlobID     string `json:"blob_id"`
	ReplyTo    string `json:"reply_to"`
	CreatedAt  int64  `json:"created_at"`
	ReceivedAt int64  `json:"received_at"`
	State      string `json:"state"`
	Deleted    bool   `json:"deleted"`
}

// AddMessage stores a message and bumps its chat.
//
// It is idempotent on id, because the mesh can legitimately deliver the same
// message twice: a flood reaches a node by two paths, or a sender retries
// something that was in fact delivered. Returning false for a duplicate lets
// the caller avoid re-notifying the user about a message they already have.
func (s *Store) AddMessage(m *Message) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("add message: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
        INSERT INTO messages
            (id, chat_id, author, mine, kind, body, blob_id, reply_to, created_at, received_at, state)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO NOTHING`,
		m.ID, m.ChatID, m.Author, boolInt(m.Mine), m.Kind, m.Body, m.BlobID,
		m.ReplyTo, m.CreatedAt, m.ReceivedAt, m.State)
	if err != nil {
		return false, fmt.Errorf("add message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add message: %w", err)
	}
	if n == 0 {
		return false, tx.Commit()
	}

	// Only an incoming message counts as unread, and only if we did not write
	// it ourselves.
	unread := 0
	if !m.Mine {
		unread = 1
	}
	if _, err := tx.Exec(`
        UPDATE chats SET last_at = MAX(last_at, ?), unread = unread + ?
        WHERE id = ?`, m.CreatedAt, unread, m.ChatID); err != nil {
		return false, fmt.Errorf("bump chat: %w", err)
	}

	return true, tx.Commit()
}

// Messages returns a page of a conversation, oldest first.
func (s *Store) Messages(chatID string, limit int, before int64) ([]*Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if before <= 0 {
		before = 1<<62 - 1
	}

	rows, err := s.db.Query(`
        SELECT id, chat_id, author, mine, kind, body, blob_id, reply_to,
               created_at, received_at, state, deleted
        FROM messages
        WHERE chat_id = ? AND created_at < ?
        ORDER BY created_at DESC
        LIMIT ?`, chatID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var out []*Message
	for rows.Next() {
		var (
			m             Message
			mine, deleted int
		)
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Author, &mine, &m.Kind, &m.Body,
			&m.BlobID, &m.ReplyTo, &m.CreatedAt, &m.ReceivedAt, &m.State, &deleted); err != nil {
			return nil, fmt.Errorf("read message: %w", err)
		}
		m.Mine, m.Deleted = mine != 0, deleted != 0
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Query descending for the LIMIT to take the newest page, hand back
	// ascending because that is reading order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Message fetches one by id.
func (s *Store) Message(id string) (*Message, error) {
	row := s.db.QueryRow(`
        SELECT id, chat_id, author, mine, kind, body, blob_id, reply_to,
               created_at, received_at, state, deleted
        FROM messages WHERE id = ?`, id)

	var (
		m             Message
		mine, deleted int
	)
	err := row.Scan(&m.ID, &m.ChatID, &m.Author, &mine, &m.Kind, &m.Body,
		&m.BlobID, &m.ReplyTo, &m.CreatedAt, &m.ReceivedAt, &m.State, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	m.Mine, m.Deleted = mine != 0, deleted != 0
	return &m, nil
}

// stateRank orders delivery states so that progress only ever moves forward.
// Acknowledgements can arrive out of order on a mesh, and a late "sent" must
// not demote a message that is already known to be read.
var stateRank = map[string]int{
	StateFailed:    0,
	StateQueued:    1,
	StateSent:      2,
	StateDelivered: 3,
	StateRead:      4,
}

// SetState advances a message's delivery state, never downgrading it.
func (s *Store) SetState(id, state string) error {
	rank, ok := stateRank[state]
	if !ok {
		return fmt.Errorf("unknown message state %q", state)
	}
	// Failure is terminal-ish and set explicitly, so it is allowed to override.
	if state == StateFailed {
		_, err := s.db.Exec(`UPDATE messages SET state = ? WHERE id = ?`, state, id)
		return err
	}

	cur, err := s.Message(id)
	if err != nil {
		return err
	}
	if cur == nil || stateRank[cur.State] >= rank {
		return nil
	}
	_, err = s.db.Exec(`UPDATE messages SET state = ? WHERE id = ?`, state, id)
	return err
}

// MarkChatRead advances every inbound message in a chat to read and returns
// the ids that changed, so receipts can be sent for exactly those.
func (s *Store) MarkChatRead(chatID string) ([]string, error) {
	rows, err := s.db.Query(`
        SELECT id FROM messages
        WHERE chat_id = ? AND mine = 0 AND state != ?`, chatID, StateRead)
	if err != nil {
		return nil, fmt.Errorf("mark read: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := s.db.Exec(`
        UPDATE messages SET state = ? WHERE chat_id = ? AND mine = 0`, StateRead, chatID); err != nil {
		return nil, err
	}
	return ids, s.MarkRead(chatID)
}

// DeleteMessage blanks a message's content locally.
//
// It cannot unsend: once a message has reached another node there is no
// authority that could compel its removal, and pretending otherwise would be a
// lie told by the interface. Deleting for everyone is a separate, cooperative
// request the other side may honour.
func (s *Store) DeleteMessage(id string) error {
	_, err := s.db.Exec(`UPDATE messages SET deleted = 1, body = '', blob_id = '' WHERE id = ?`, id)
	return err
}

// Undelivered lists messages still waiting to leave, so a restart can resume
// sending rather than silently losing them.
func (s *Store) Undelivered() ([]*Message, error) {
	rows, err := s.db.Query(`
        SELECT id, chat_id, author, mine, kind, body, blob_id, reply_to,
               created_at, received_at, state, deleted
        FROM messages
        WHERE mine = 1 AND state IN (?, ?)
        ORDER BY created_at ASC`, StateQueued, StateSent)
	if err != nil {
		return nil, fmt.Errorf("list undelivered: %w", err)
	}
	defer rows.Close()

	var out []*Message
	for rows.Next() {
		var (
			m             Message
			mine, deleted int
		)
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Author, &mine, &m.Kind, &m.Body,
			&m.BlobID, &m.ReplyTo, &m.CreatedAt, &m.ReceivedAt, &m.State, &deleted); err != nil {
			return nil, err
		}
		m.Mine, m.Deleted = mine != 0, deleted != 0
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Blobs
// ---------------------------------------------------------------------------

// PutBlob stores binary content and returns its id.
func (s *Store) PutBlob(id, mime, name string, data []byte) error {
	_, err := s.db.Exec(`
        INSERT INTO blobs (id, mime, name, bytes) VALUES (?, ?, ?, ?)
        ON CONFLICT(id) DO NOTHING`, id, mime, name, data)
	if err != nil {
		return fmt.Errorf("store blob: %w", err)
	}
	return nil
}

// Blob fetches binary content.
func (s *Store) Blob(id string) (mime, name string, data []byte, err error) {
	row := s.db.QueryRow(`SELECT mime, name, bytes FROM blobs WHERE id = ?`, id)
	err = row.Scan(&mime, &name, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil, nil
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("read blob: %w", err)
	}
	return mime, name, data, nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// Setting reads a value, returning def when unset.
func (s *Store) Setting(key, def string) string {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil || v == "" {
		return def
	}
	return v
}

// SetSetting writes a value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
        INSERT INTO settings (key, value) VALUES (?, ?)
        ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NodeIDHex is a small helper for callers holding a raw node id.
func NodeIDHex(n identity.NodeID) string { return hex.EncodeToString(n[:]) }
