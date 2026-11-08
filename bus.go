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
	"os/exec"
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

CREATE TABLE IF NOT EXISTS watchers (
    session_id TEXT PRIMARY KEY,
    pid        INTEGER NOT NULL
);

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

// send delivers to Codex through its own queue and to Claude through the inbox.
func (b *Bus) send(from, to, prompt string, meta map[string]any) (string, error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	message := Message{ID: hex.EncodeToString(raw), From: from, Prompt: prompt, Meta: encoded, CreatedAt: now()}
	agent, err := b.agentOf(to)
	if err != nil {
		return "", err
	}
	if agent == "codex" {
		var text bytes.Buffer
		if err := printJSON(&text, []Message{message}); err != nil {
			return "", err
		}
		return message.ID, codexQueue(to, text.String())
	}
	_, err = b.db.Exec(
		`INSERT INTO messages (id, "to", "from", prompt, meta, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		message.ID, to, from, prompt, string(encoded), message.CreatedAt,
	)
	return message.ID, err
}

// agentOf reads the agent from the index. A session that is not indexed yet is
// told by its id: Codex ids are UUID version 7, Claude ids version 4.
func (b *Bus) agentOf(session string) (string, error) {
	var agent string
	err := b.db.QueryRow(`SELECT agent FROM history WHERE session_id = ? LIMIT 1`, session).Scan(&agent)
	if err == nil {
		return agent, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if len(session) > 14 && session[14] == '7' {
		return "codex", nil
	}
	return "claude", nil
}

// codexQueue puts text into a Codex session as a user message.
var codexQueue = func(thread, text string) error {
	output, err := exec.Command("codex", "queue", "--thread", thread, "--message", text).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex queue: %w: %s", err, bytes.TrimSpace(output))
	}
	return nil
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

// watch polls the inbox of session until mail arrives, and returns it. A newer
// watcher of the same session replaces this one, and then watch returns nil.
func (b *Bus) watch(session string, pid int, interval time.Duration) ([]Message, error) {
	_, err := b.db.Exec(`INSERT INTO watchers (session_id, pid) VALUES (?, ?)
        ON CONFLICT (session_id) DO UPDATE SET pid = excluded.pid`, session, pid)
	if err != nil {
		return nil, err
	}
	for {
		var current int
		if err := b.db.QueryRow(`SELECT pid FROM watchers WHERE session_id = ?`, session).Scan(&current); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if current != pid {
			return nil, nil
		}
		messages, err := b.inbox(session)
		if err != nil {
			return nil, err
		}
		if len(messages) > 0 {
			_, err := b.db.Exec(`DELETE FROM watchers WHERE session_id = ? AND pid = ?`, session, pid)
			return messages, err
		}
		time.Sleep(interval)
	}
}
