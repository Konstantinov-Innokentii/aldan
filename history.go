package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// The index holds typed prompts and final answers from both harnesses. The
// transcripts are its only input, and a changed transcript is read again in
// full. Rows are insert-only, so they outlive transcripts that get pruned.

var (
	questionReply = regexp.MustCompile(`(?s)^<send_user_message_question_reply>(.*)</send_user_message_question_reply>$`)
	commandName   = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs   = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// Sources are the transcript roots.
type Sources struct {
	Claude string
	Codex  string
}

// row is one history row. Empty strings are stored as NULL.
type row struct {
	id, session, role, text, ts, cwd, origin, parent, agent string
}

type record = map[string]any

func str(r record, key string) string {
	value, _ := r[key].(string)
	return value
}

func obj(r record, key string) record {
	value, _ := r[key].(record)
	return value
}

func isTrue(r record, key string) bool {
	value, _ := r[key].(bool)
	return value
}

// blocks returns the object items of a content list.
func blocks(content any) []record {
	items, _ := content.([]any)
	var found []record
	for _, item := range items {
		if block, ok := item.(record); ok {
			found = append(found, block)
		}
	}
	return found
}

func blockText(content any, kind string) string {
	var parts []string
	for _, block := range blocks(content) {
		if str(block, "type") == kind {
			parts = append(parts, str(block, "text"))
		}
	}
	return strings.Join(parts, "\n")
}

// contentText reads Claude content: a string, or a list of blocks.
func contentText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	return blockText(content, "text")
}

// eachRecord calls fn for every JSON object in a transcript. A malformed line is
// one still being written, and the next sync gets it. It reads with ReadBytes
// because lines can outgrow a Scanner buffer. fn returns false to stop.
func eachRecord(path string, fn func(record) bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		var r record
		if json.Unmarshal(line, &r) == nil && r != nil && !fn(r) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func hasToolResult(r record) bool {
	for _, block := range blocks(obj(r, "message")["content"]) {
		if str(block, "type") == "tool_result" {
			return true
		}
	}
	return false
}

// unwrapCommand turns a typed skill call, which is wrapped in tags, into "<name> <args>".
func unwrapCommand(text string) string {
	if !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<command-") {
		return text
	}
	name := commandName.FindStringSubmatch(text)
	if name == nil {
		return text
	}
	args := ""
	if match := commandArgs.FindStringSubmatch(text); match != nil {
		args = match[1]
	}
	return strings.TrimSpace(strings.TrimSpace(name[1]) + " " + strings.TrimSpace(args))
}

// typedText returns the user's text, if this record is typed input.
func typedText(r record) (string, bool) {
	switch str(r, "type") {
	case "attachment":
		// A message typed during a turn is stored only this way.
		attachment := obj(r, "attachment")
		if str(attachment, "type") == "queued_command" && str(attachment, "commandMode") == "prompt" &&
			str(obj(attachment, "origin"), "kind") == "human" {
			return unwrapCommand(contentText(attachment["prompt"])), true
		}
	case "user":
		if !isTrue(r, "isMeta") && !hasToolResult(r) && str(obj(r, "origin"), "kind") == "human" {
			return unwrapCommand(contentText(obj(r, "message")["content"])), true
		}
	}
	return "", false
}

// answerText returns the final answer in this record. Stopping with tool_use
// means commentary.
func answerText(r record) (string, bool) {
	message := obj(r, "message")
	if str(r, "type") != "assistant" || str(message, "stop_reason") != "end_turn" || str(message, "model") == "<synthetic>" {
		return "", false
	}
	return contentText(message["content"]), true
}

// turnKind says what started the turn, if this record started one.
func turnKind(r record) string {
	if str(r, "type") != "user" || isTrue(r, "isCompactSummary") || hasToolResult(r) {
		return ""
	}
	if kind := str(obj(r, "origin"), "kind"); kind != "" {
		return kind
	}
	return str(r, "turnOrigin")
}

// node is what the ancestry walk needs from a record. Keeping only this, not
// the decoded records, keeps memory small on long transcripts.
type node struct {
	up        string
	turnKind  string
	assistant bool
}

// ancestry walks the recorded links up from id. It stops at a missing link or a
// cycle and never infers anything from file order, so unknowns stay empty.
func ancestry(id string, nodes map[string]node, indexed map[string]bool) (origin, parent string) {
	seen := map[string]bool{id: true}
	for up := nodes[id].up; up != "" && !seen[up]; up = nodes[up].up {
		ancestor, known := nodes[up]
		if !known {
			break
		}
		seen[up] = true
		if origin == "" {
			origin = ancestor.turnKind
		}
		if indexed[up] {
			if parent == "" {
				parent = up
			}
			if ancestor.assistant {
				break
			}
		}
		if origin != "" && parent != "" {
			break
		}
	}
	return origin, parent
}

func readClaude(path string) ([]row, error) {
	nodes := map[string]node{}
	var candidates []row
	err := eachRecord(path, func(r record) bool {
		id := str(r, "uuid")
		if id == "" || isTrue(r, "isSidechain") {
			return true
		}
		up := str(r, "parentUuid")
		if up == "" {
			up = str(r, "logicalParentUuid")
		}
		nodes[id] = node{up: up, turnKind: turnKind(r), assistant: str(r, "type") == "assistant"}

		role := "user"
		text, ok := typedText(r)
		if !ok {
			role = "assistant"
			text, ok = answerText(r)
		}
		if ok && strings.TrimSpace(text) != "" {
			candidates = append(candidates, row{
				id: id, session: str(r, "sessionId"), role: role, text: text,
				ts: str(r, "timestamp"), cwd: str(r, "cwd"), agent: "claude",
			})
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	var rows []row
	indexed := map[string]bool{}
	for _, candidate := range candidates {
		candidate.origin, candidate.parent = ancestry(candidate.id, nodes, indexed)
		if candidate.role == "user" {
			candidate.origin = "human"
		} else if candidate.origin == "sdk" {
			continue // a scripted run, with no user
		}
		indexed[candidate.id] = true
		rows = append(rows, candidate)
	}
	return rows, nil
}

// codexTyped keeps only the answers when the message replies to a Codex question.
func codexTyped(text string) string {
	reply := questionReply.FindStringSubmatch(text)
	if reply == nil {
		return text
	}
	var items []struct {
		Answer *string `json:"answer"`
	}
	if json.Unmarshal([]byte(reply[1]), &items) != nil {
		return text
	}
	var answers []string
	for _, item := range items {
		if item.Answer == nil {
			return text
		}
		answers = append(answers, *item.Answer)
	}
	return strings.Join(answers, "\n")
}

// codexRow returns the role and text if this record is typed input or a final answer.
func codexRow(r record) (role, text string) {
	payload := obj(r, "payload")
	kind := str(payload, "type")
	switch str(r, "type") {
	case "response_item":
		if kind == "message" && str(payload, "phase") == "final_answer" {
			return "assistant", blockText(payload["content"], "output_text")
		}
	case "event_msg":
		if kind == "user_message" {
			return "user", codexTyped(str(payload, "message"))
		}
		if item := obj(payload, "item"); kind == "item_completed" && str(item, "type") == "UserMessage" {
			return "user", codexTyped(blockText(item["content"], "text"))
		}
	}
	return "", ""
}

// readCodex skips scripted and subagent runs, which are not conversations with
// the user.
func readCodex(path string) ([]row, error) {
	var meta record
	var rows []row
	err := eachRecord(path, func(r record) bool {
		if meta == nil {
			if str(r, "type") != "session_meta" {
				return true
			}
			meta = obj(r, "payload")
			source := str(meta, "source")
			return (source == "cli" || source == "vscode") && str(meta, "originator") != "Claude Code"
		}
		role, text := codexRow(r)
		if strings.TrimSpace(text) == "" {
			return true
		}
		session, ts := str(meta, "id"), str(r, "timestamp")
		// Rollouts have no record ids, so the content is the identity.
		digest := sha256.Sum256([]byte(session + "\x00" + ts + "\x00" + role + "\x00" + text))
		origin := ""
		if role == "user" {
			origin = "human"
		}
		rows = append(rows, row{
			id: "codex:" + hex.EncodeToString(digest[:]), session: session, role: role, text: text,
			ts: ts, cwd: str(meta, "cwd"), origin: origin, agent: "codex",
		})
		return true
	})
	return rows, err
}

type transcript struct {
	path   string
	size   int64
	mtime  int64
	reader func(string) ([]row, error)
}

func transcripts(sources Sources) ([]transcript, error) {
	var found []transcript
	add := func(path string, reader func(string) ([]row, error)) {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			found = append(found, transcript{path, info.Size(), info.ModTime().UnixNano(), reader})
		}
	}
	// Exactly one level deep: subagent logs live further down.
	projects, err := os.ReadDir(sources.Claude)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, project := range projects {
		dir := filepath.Join(sources.Claude, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // not a directory, or removed since
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".jsonl") {
				add(filepath.Join(dir, entry.Name()), readClaude)
			}
		}
	}
	err = filepath.WalkDir(sources.Codex, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "skipped %s: %v\n", path, err)
			}
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			add(path, readCodex)
		}
		return nil
	})
	return found, err
}

func (b *Bus) knownFiles() (map[string][2]int64, error) {
	rows, err := b.db.Query(`SELECT path, size, mtime_ns FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string][2]int64{}
	for rows.Next() {
		var path string
		var size, mtime int64
		if err := rows.Scan(&path, &size, &mtime); err != nil {
			return nil, err
		}
		known[path] = [2]int64{size, mtime}
	}
	return known, rows.Err()
}

// sync reads changed transcripts and returns the number of rows added.
func (b *Bus) sync(sources Sources) (int, error) {
	found, err := transcripts(sources)
	if err != nil {
		return 0, err
	}
	known, err := b.knownFiles()
	if err != nil {
		return 0, err
	}
	var changed []transcript
	for _, t := range found {
		if seen, ok := known[t.path]; !ok || seen != [2]int64{t.size, t.mtime} {
			changed = append(changed, t)
		}
	}
	// Oldest first: a resumed session copies old records under the same uuid, and
	// those stay with the session where they first happened.
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].mtime != changed[j].mtime {
			return changed[i].mtime < changed[j].mtime
		}
		return changed[i].path < changed[j].path
	})
	added := 0
	for _, t := range changed {
		count, err := b.indexFile(t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", t.path, err)
			continue
		}
		added += count
	}
	return added, nil
}

func nullable(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// indexFile parses before it opens the transaction, so the write lock is held
// only while rows go in.
func (b *Bus) indexFile(t transcript) (int, error) {
	rows, err := t.reader(t.path)
	if err != nil {
		return 0, err
	}
	tx, err := b.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, r := range rows {
		result, err := tx.Exec(
			`INSERT OR IGNORE INTO history (id, session_id, role, text, ts, cwd, origin, parent_id, agent)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, nullable(r.session), r.role, r.text, nullable(r.ts), nullable(r.cwd),
			nullable(r.origin), nullable(r.parent), r.agent,
		)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		added += int(count)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO files (path, size, mtime_ns) VALUES (?, ?, ?)`, t.path, t.size, t.mtime); err != nil {
		return 0, err
	}
	return added, tx.Commit()
}

// search runs any SQL against the index. The connection is read-only at the
// file level, so a query cannot turn that off. ATTACH is disabled because it
// could open the same file again for writing.
func search(path, query string) ([]map[string]any, error) {
	db, err := connect(path, "mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_ATTACHED, 0); err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	found := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		item := map[string]any{}
		for i, column := range columns {
			if text, ok := values[i].([]byte); ok {
				item[column] = string(text)
			} else {
				item[column] = values[i]
			}
		}
		found = append(found, item)
	}
	return found, rows.Err()
}
