package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  chat_jid   TEXT NOT NULL,
  msg_id     TEXT NOT NULL,
  sender_jid TEXT,
  from_me    INTEGER NOT NULL DEFAULT 0,
  timestamp  INTEGER NOT NULL,
  text       TEXT,
  PRIMARY KEY (chat_jid, msg_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages(chat_jid, timestamp DESC);

CREATE TABLE IF NOT EXISTS chats (
  jid     TEXT PRIMARY KEY,
  name    TEXT,
  last_ts INTEGER
);

CREATE TABLE IF NOT EXISTS journal (
  id              TEXT PRIMARY KEY,
  idempotency_key TEXT UNIQUE,
  action          TEXT NOT NULL,
  chat_jid        TEXT,
  payload         TEXT,
  status          TEXT NOT NULL,
  error           TEXT,
  dry_run         INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_journal_created ON journal(created_at DESC);
`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialize writes; SQLite + single daemon
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// Migration: media_path holds the on-disk path of downloaded media for a
	// message. Idempotent — ignore the "duplicate column" error on re-open.
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN media_path TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---- messages / chats ----

type MessageRow struct {
	ChatJID    string `json:"chat_jid"`
	MsgID      string `json:"msg_id"`
	SenderJID  string `json:"sender_jid"`
	SenderName string `json:"sender_name,omitempty"`
	FromMe     bool   `json:"from_me"`
	Timestamp  int64  `json:"timestamp"`
	Text       string `json:"text"`
	MediaPath  string `json:"media_path,omitempty"`
}

// SetMediaPath records where a message's downloaded media was saved. Called
// after the initial text upsert, once the download completes.
func (s *Store) SetMediaPath(chatJID, msgID, path string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET media_path=? WHERE chat_jid=? AND msg_id=?`,
		path, chatJID, msgID)
	return err
}

func (s *Store) UpsertMessage(m MessageRow) error {
	_, err := s.db.Exec(`
		INSERT INTO messages (chat_jid, msg_id, sender_jid, from_me, timestamp, text)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, msg_id) DO UPDATE SET
		  sender_jid=excluded.sender_jid, timestamp=excluded.timestamp,
		  text=CASE WHEN excluded.text != '' THEN excluded.text ELSE messages.text END`,
		m.ChatJID, m.MsgID, m.SenderJID, boolToInt(m.FromMe), m.Timestamp, m.Text)
	return err
}

func (s *Store) TouchChat(jid, name string, lastTS int64) error {
	_, err := s.db.Exec(`
		INSERT INTO chats (jid, name, last_ts) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
		  name=CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
		  last_ts=MAX(COALESCE(chats.last_ts,0), excluded.last_ts)`,
		jid, name, lastTS)
	return err
}

func (s *Store) LastMessage(chatJID string) (*MessageRow, error) {
	row := s.db.QueryRow(`
		SELECT chat_jid, msg_id, COALESCE(sender_jid,''), from_me, timestamp, COALESCE(text,''), COALESCE(media_path,'')
		FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT 1`, chatJID)
	var m MessageRow
	var fromMe int
	err := row.Scan(&m.ChatJID, &m.MsgID, &m.SenderJID, &fromMe, &m.Timestamp, &m.Text, &m.MediaPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.FromMe = fromMe == 1
	return &m, nil
}

type ChatRow struct {
	JID    string `json:"jid"`
	Name   string `json:"name"`
	LastTS int64  `json:"last_ts"`
}

func (s *Store) ListChats(limit int) ([]ChatRow, error) {
	rows, err := s.db.Query(`
		SELECT jid, COALESCE(name,''), COALESCE(last_ts,0)
		FROM chats ORDER BY last_ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatRow
	for rows.Next() {
		var c ChatRow
		if err := rows.Scan(&c.JID, &c.Name, &c.LastTS); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetMessages(chatJID string, limit int) ([]MessageRow, error) {
	rows, err := s.db.Query(`
		SELECT chat_jid, msg_id, COALESCE(sender_jid,''), from_me, timestamp, COALESCE(text,''), COALESCE(media_path,'')
		FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?`, chatJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var fromMe int
		if err := rows.Scan(&m.ChatJID, &m.MsgID, &m.SenderJID, &fromMe, &m.Timestamp, &m.Text, &m.MediaPath); err != nil {
			return nil, err
		}
		m.FromMe = fromMe == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveChat prunes a chat and its messages from the local index. Called after
// a successful delete so readback reflects the mutation immediately.
func (s *Store) RemoveChat(jid string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE chat_jid = ?`, jid); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM chats WHERE jid = ?`, jid)
	return err
}

func (s *Store) ChatName(jid string) string {
	var name sql.NullString
	_ = s.db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	return name.String
}

// ---- journal / idempotency ----

type JournalEntry struct {
	ID             string          `json:"id"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Action         string          `json:"action"`
	ChatJID        string          `json:"chat_jid,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Status         string          `json:"status"`
	Error          string          `json:"error,omitempty"`
	DryRun         bool            `json:"dry_run"`
	CreatedAt      int64           `json:"created_at"`
	UpdatedAt      int64           `json:"updated_at"`
}

// BeginAction records an accepted action. If the idempotency key was already
// used, returns the existing entry and replayed=true.
func (s *Store) BeginAction(action, chatJID, idemKey string, payload any, dryRun bool) (*JournalEntry, bool, error) {
	if idemKey != "" {
		existing, err := s.GetActionByIdemKey(idemKey)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return existing, true, nil
		}
	}
	raw, _ := json.Marshal(payload)
	now := time.Now().UnixMilli()
	e := &JournalEntry{
		ID:             uuid.NewString(),
		IdempotencyKey: idemKey,
		Action:         action,
		ChatJID:        chatJID,
		Payload:        raw,
		Status:         "accepted",
		DryRun:         dryRun,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	var idem any
	if idemKey != "" {
		idem = idemKey
	}
	_, err := s.db.Exec(`
		INSERT INTO journal (id, idempotency_key, action, chat_jid, payload, status, dry_run, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, idem, e.Action, e.ChatJID, string(raw), e.Status, boolToInt(dryRun), now, now)
	if err != nil {
		return nil, false, err
	}
	return e, false, nil
}

func (s *Store) FinishAction(id, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE journal SET status=?, error=?, updated_at=? WHERE id=?`,
		status, errMsg, time.Now().UnixMilli(), id)
	return err
}

func (s *Store) GetAction(id string) (*JournalEntry, error) {
	return s.scanJournalRow(s.db.QueryRow(`
		SELECT id, COALESCE(idempotency_key,''), action, COALESCE(chat_jid,''), COALESCE(payload,''),
		       status, COALESCE(error,''), dry_run, created_at, updated_at
		FROM journal WHERE id = ?`, id))
}

func (s *Store) GetActionByIdemKey(key string) (*JournalEntry, error) {
	return s.scanJournalRow(s.db.QueryRow(`
		SELECT id, COALESCE(idempotency_key,''), action, COALESCE(chat_jid,''), COALESCE(payload,''),
		       status, COALESCE(error,''), dry_run, created_at, updated_at
		FROM journal WHERE idempotency_key = ?`, key))
}

func (s *Store) scanJournalRow(row *sql.Row) (*JournalEntry, error) {
	var e JournalEntry
	var payload string
	var dryRun int
	err := row.Scan(&e.ID, &e.IdempotencyKey, &e.Action, &e.ChatJID, &payload,
		&e.Status, &e.Error, &dryRun, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.DryRun = dryRun == 1
	if payload != "" {
		e.Payload = json.RawMessage(payload)
	}
	return &e, nil
}

func (s *Store) CountMessages() (int64, int64) {
	var msgs, chats int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&chats)
	return msgs, chats
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var _ = fmt.Sprintf // keep fmt import when unused paths compile out
