package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id           TEXT PRIMARY KEY,
    "to"         TEXT NOT NULL,
    "from"       TEXT NOT NULL,
    prompt       TEXT NOT NULL,
    meta         TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    delivered_at TEXT
);
CREATE INDEX IF NOT EXISTS messages_pending ON messages("to") WHERE delivered_at IS NULL;

CREATE TABLE IF NOT EXISTS history (
    id         TEXT PRIMARY KEY,
    session_id TEXT,
    role       TEXT NOT NULL,
    text       TEXT NOT NULL,
    ts         TEXT,
    cwd        TEXT,
    origin     TEXT,
    parent_id  TEXT,
    agent      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS history_session ON history(session_id);

CREATE TABLE IF NOT EXISTS files (
    path     TEXT PRIMARY KEY,
    size     INTEGER NOT NULL,
    mtime_ns INTEGER NOT NULL
);`

// Bus is the one SQLite file that every session's CLI shares.
type Bus struct {
	db *sql.DB
}

// Message is one item of `aldan inbox` output.
type Message struct {
	ID        string          `json:"id"`
	From      string          `json:"from"`
	Prompt    string          `json:"prompt"`
	Meta      json.RawMessage `json:"meta"`
	CreatedAt string          `json:"created_at"`
}

// open creates the database if needed. Conversations can hold secrets, so the
// file stays private to the user whatever its mode was.
func open(path string) (*Bus, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	file.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := connect(path, "_pragma=busy_timeout(30000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Bus{db: db}, nil
}

func connect(path, options string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: options}
	return sql.Open("sqlite", dsn.String())
}

func (b *Bus) Close() error { return b.db.Close() }

// timeLayout has a fixed width, so that timestamps sort correctly as strings.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func now() string { return time.Now().UTC().Format(timeLayout) }

// decodeObject parses one JSON object. Numbers keep their exact text.
func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("not an object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after the object")
	}
	return object, nil
}

func (b *Bus) send(from, to, prompt string, meta map[string]any) (string, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	_, err = b.db.Exec(
		`INSERT INTO messages (id, "to", "from", prompt, meta, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, to, from, prompt, string(encoded), now(),
	)
	return id, err
}

// inbox is one UPDATE, so two readers cannot get the same message.
func (b *Bus) inbox(session string) ([]Message, error) {
	rows, err := b.db.Query(`
        UPDATE messages SET delivered_at = ?
        WHERE "to" = ? AND delivered_at IS NULL
        RETURNING id, "from", prompt, meta, created_at`, now(), session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var message Message
		var meta string
		if err := rows.Scan(&message.ID, &message.From, &message.Prompt, &meta, &message.CreatedAt); err != nil {
			return nil, err
		}
		message.Meta = json.RawMessage(meta)
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].CreatedAt < messages[j].CreatedAt })
	return messages, nil
}
