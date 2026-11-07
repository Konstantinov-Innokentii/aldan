package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type rec = map[string]any

func writeRecords(t *testing.T, path string, records ...rec) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeRows(t *testing.T, records ...rec) []row {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeRecords(t, path, records...)
	rows, err := readClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func codexRows(t *testing.T, records ...rec) []row {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeRecords(t, path, records...)
	rows, err := readCodex(path)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func merge(base rec, extra ...rec) rec {
	merged := rec{}
	for key, value := range base {
		merged[key] = value
	}
	for _, r := range extra {
		for key, value := range r {
			merged[key] = value
		}
	}
	return merged
}

func prompt(id, parent, text string, extra ...rec) rec {
	return merge(rec{
		"type": "user", "uuid": id, "parentUuid": parent, "sessionId": "s", "cwd": "/w",
		"timestamp": "2026-01-01T00:00:00Z", "origin": rec{"kind": "human"},
		"message": rec{"content": text},
	}, extra...)
}

func answer(id, parent, text string, extra ...rec) rec {
	return merge(rec{
		"type": "assistant", "uuid": id, "parentUuid": parent, "sessionId": "s",
		"timestamp": "2026-01-01T00:00:01Z",
		"message": rec{"stop_reason": "end_turn", "model": "m",
			"content": []any{rec{"type": "text", "text": text}}},
	}, extra...)
}

func commentary(id, parent string) rec {
	return rec{"type": "assistant", "uuid": id, "parentUuid": parent, "sessionId": "s",
		"message": rec{"stop_reason": "tool_use", "model": "m",
			"content": []any{rec{"type": "text", "text": "working on it"}}}}
}

func toolResult(id, parent string) rec {
	return rec{"type": "user", "uuid": id, "parentUuid": parent, "sessionId": "s",
		"message": rec{"content": []any{rec{"type": "tool_result", "content": "output"}}}}
}

func queued(id, parent, text, kind string) rec {
	return rec{"type": "attachment", "uuid": id, "parentUuid": parent, "sessionId": "s",
		"attachment": rec{"type": "queued_command", "commandMode": "prompt", "prompt": text,
			"origin": rec{"kind": kind}}}
}

func texts(rows []row) []string {
	found := []string{}
	for _, r := range rows {
		found = append(found, r.text)
	}
	return found
}

func expectTexts(t *testing.T, rows []row, want ...string) {
	t.Helper()
	if got := texts(rows); fmt.Sprint(got) != fmt.Sprint(append([]string{}, want...)) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}

func TestClaudeKeepsPromptAndFinalAnswer(t *testing.T) {
	rows := claudeRows(t,
		prompt("u1", "", "do it"),
		commentary("a1", "u1"),
		toolResult("t1", "a1"),
		answer("a2", "t1", "done"),
	)
	expectTexts(t, rows, "do it", "done")
	want := row{id: "u1", session: "s", role: "user", text: "do it", ts: "2026-01-01T00:00:00Z", cwd: "/w", origin: "human", agent: "claude"}
	if rows[0] != want {
		t.Fatalf("user row = %+v", rows[0])
	}
	if rows[1].role != "assistant" || rows[1].origin != "human" || rows[1].parent != "u1" {
		t.Fatalf("assistant row = %+v", rows[1])
	}
}

func TestClaudeExclusions(t *testing.T) {
	noStop := answer("x4", "u1", "partial")
	noStop["message"].(rec)["stop_reason"] = nil
	synthetic := answer("x6", "u1", "synthetic")
	synthetic["message"].(rec)["model"] = "<synthetic>"
	noUUID := prompt("", "", "no uuid")
	delete(noUUID, "uuid")
	noOrigin := prompt("x2", "", "no origin")
	delete(noOrigin, "origin")

	rows := claudeRows(t,
		prompt("u1", "", "kept"),
		prompt("x1", "u1", "meta", rec{"isMeta": true}),
		prompt("x0", "u1", "   \n"),
		noOrigin,
		noUUID,
		prompt("x3", "u1", "<task-notification>", rec{"origin": rec{"kind": "task-notification"}}),
		noStop,
		answer("x5", "u1", "  "),
		synthetic,
		prompt("x7", "u1", "side", rec{"isSidechain": true}),
		answer("x8", "x7", "side answer", rec{"isSidechain": true}),
	)
	expectTexts(t, rows, "kept")
}

func TestClaudeScriptedRunIsExcluded(t *testing.T) {
	scripted := prompt("u1", "", "run this", rec{"turnOrigin": "sdk"})
	delete(scripted, "origin")
	expectTexts(t, claudeRows(t, scripted, answer("a1", "u1", "ran")))
}

func TestClaudePeerAnswerIsIndexed(t *testing.T) {
	rows := claudeRows(t,
		prompt("u1", "", "hello"),
		answer("a1", "u1", "hi"),
		prompt("p1", "a1", "from a peer", rec{"isMeta": true, "origin": rec{"kind": "peer"}}),
		answer("a2", "p1", "peer answer"),
	)
	expectTexts(t, rows, "hello", "hi", "peer answer")
	if rows[2].origin != "peer" {
		t.Fatalf("origin = %q", rows[2].origin)
	}
	// The peer prompt is not indexed, so the nearest indexed ancestor is further up.
	if rows[2].parent != "a1" {
		t.Fatalf("parent = %q", rows[2].parent)
	}
}

func TestClaudeTaskNotificationAnswerKeepsOrigin(t *testing.T) {
	rows := claudeRows(t,
		prompt("n1", "", "<task-notification>", rec{"origin": rec{"kind": "task-notification"}}),
		answer("a1", "n1", "handled"),
	)
	expectTexts(t, rows, "handled")
	if rows[0].origin != "task-notification" || rows[0].parent != "" {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestClaudeQueuedAttachment(t *testing.T) {
	rows := claudeRows(t,
		prompt("u1", "", "first"),
		commentary("c1", "u1"),
		queued("q1", "c1", "typed mid-turn", "human"),
		queued("q2", "q1", "from a peer", "peer"),
		toolResult("t1", "q2"),
		answer("a1", "t1", "done"),
	)
	expectTexts(t, rows, "first", "typed mid-turn", "done")
	if rows[1].origin != "human" || rows[1].parent != "u1" {
		t.Fatalf("attachment row = %+v", rows[1])
	}
	if rows[2].parent != "q1" || rows[2].origin != "human" {
		t.Fatalf("answer row = %+v", rows[2])
	}
}

func TestClaudeSkillCall(t *testing.T) {
	rows := claudeRows(t,
		prompt("u1", "", "\n<command-message>x</command-message>\n<command-name>/x</command-name>\n<command-args>build it</command-args>"),
		prompt("u2", "u1", "<command-name>/x</command-name>\n<command-message>x</command-message>\n<command-args></command-args>"),
		prompt("u3", "u2", "<command-name>/y</command-name>"),
		prompt("u4", "u3", "text about <command-name>/x</command-name>"),
	)
	expectTexts(t, rows, "/x build it", "/x", "/y", "text about <command-name>/x</command-name>")
}

func TestClaudeCompaction(t *testing.T) {
	rows := claudeRows(t,
		prompt("u1", "", "original"),
		rec{"type": "system", "uuid": "b1", "parentUuid": nil, "logicalParentUuid": "u1", "sessionId": "s"},
		rec{"type": "user", "uuid": "c1", "parentUuid": "b1", "sessionId": "s", "isCompactSummary": true,
			"turnOrigin": "human", "message": rec{"content": "summary"}},
		answer("a1", "c1", "after compaction"),
	)
	expectTexts(t, rows, "original", "after compaction")
	if rows[1].origin != "human" || rows[1].parent != "u1" {
		t.Fatalf("row = %+v", rows[1])
	}
}

func TestClaudeBrokenAncestry(t *testing.T) {
	for name, records := range map[string][]rec{
		"missing": {answer("a1", "gone", "answer")},
		"cycle": {
			{"type": "system", "uuid": "b", "parentUuid": "c", "sessionId": "s"},
			{"type": "system", "uuid": "c", "parentUuid": "b", "sessionId": "s"},
			answer("a1", "b", "answer"),
		},
		"self": {answer("a1", "a1", "answer")},
	} {
		t.Run(name, func(t *testing.T) {
			rows := claudeRows(t, records...)
			expectTexts(t, rows, "answer")
			if rows[0].origin != "" || rows[0].parent != "" {
				t.Fatalf("row = %+v", rows[0])
			}
		})
	}
}

func TestClaudeWalkStopsAtIndexedAssistant(t *testing.T) {
	rows := claudeRows(t,
		prompt("u1", "", "hello"),
		answer("a1", "u1", "first"),
		answer("a2", "a1", "second"),
	)
	if rows[2].parent != "a1" || rows[2].origin != "" {
		t.Fatalf("row = %+v", rows[2])
	}
}

func TestClaudeLineLargerThanScannerBuffer(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	expectTexts(t, claudeRows(t, prompt("u1", "", big)), big)
}

func newBus(t *testing.T) *Bus {
	t.Helper()
	bus, err := open(filepath.Join(t.TempDir(), "data", "aldan.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	return bus
}

func newSources(t *testing.T) Sources {
	t.Helper()
	root := t.TempDir()
	return Sources{Claude: filepath.Join(root, "claude"), Codex: filepath.Join(root, "codex")}
}

func expectSync(t *testing.T, bus *Bus, sources Sources, want int) {
	t.Helper()
	got, err := bus.sync(sources)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("sync added %d, want %d", got, want)
	}
}

func historyCount(t *testing.T, bus *Bus) int {
	t.Helper()
	var count int
	if err := bus.db.QueryRow(`SELECT COUNT(*) FROM history`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOpenIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "aldan.sqlite3")
	bus, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	for target, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", target, info.Mode().Perm(), want)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("reopened mode = %o", info.Mode().Perm())
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	bus, err := open(filepath.Join(t.TempDir(), "a b?c#d", "aldan.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	bus.Close()
}

func TestOpenSettings(t *testing.T) {
	bus := newBus(t)
	var timeout int
	if err := bus.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 30000 {
		t.Fatalf("busy_timeout = %d", timeout)
	}
}

func TestSyncMalformedTail(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	path := filepath.Join(sources.Claude, "p", "s.jsonl")
	writeRecords(t, path, prompt("u1", "", "one"))
	expectSync(t, bus, sources, 1)

	whole, err := json.Marshal(answer("a1", "u1", "two"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	half := len(whole) / 2
	if _, err := file.Write(whole[:half]); err != nil {
		t.Fatal(err)
	}
	expectSync(t, bus, sources, 0)
	// Complete, and still with no newline at the end.
	if _, err := file.Write(whole[half:]); err != nil {
		t.Fatal(err)
	}
	expectSync(t, bus, sources, 1)
}

func TestSyncIsIdempotentAndDeduplicates(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	records := []rec{prompt("u1", "", "one"), answer("a1", "u1", "two")}
	writeRecords(t, filepath.Join(sources.Claude, "p", "a.jsonl"), records...)
	writeRecords(t, filepath.Join(sources.Claude, "p", "b.jsonl"), records...)
	expectSync(t, bus, sources, 2)
	expectSync(t, bus, sources, 0)
}

func TestSyncRowsOutliveTranscripts(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	path := filepath.Join(sources.Claude, "p", "s.jsonl")
	writeRecords(t, path, prompt("u1", "", "one"))
	expectSync(t, bus, sources, 1)
	writeRecords(t, path)
	expectSync(t, bus, sources, 0)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expectSync(t, bus, sources, 0)
	if got := historyCount(t, bus); got != 1 {
		t.Fatalf("history has %d rows", got)
	}
}

func TestSyncDoesNotReadSubagents(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	writeRecords(t, filepath.Join(sources.Claude, "p", "sess", "subagents", "agent.jsonl"), prompt("u1", "", "one"))
	writeRecords(t, filepath.Join(sources.Claude, "p", "sess", "agent.jsonl"), prompt("u2", "", "two"))
	writeRecords(t, filepath.Join(sources.Claude, "top.jsonl"), prompt("u3", "", "three"))
	expectSync(t, bus, sources, 0)
}

func TestSyncMissingRoots(t *testing.T) {
	expectSync(t, newBus(t), newSources(t), 0)
}

func TestSyncResumedRecordsStayWithFirstSession(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	older := filepath.Join(sources.Claude, "p", "z-old.jsonl")
	newer := filepath.Join(sources.Claude, "p", "a-new.jsonl")
	writeRecords(t, older, prompt("u1", "", "one", rec{"sessionId": "first"}))
	writeRecords(t, newer, prompt("u1", "", "one", rec{"sessionId": "resumed"}))
	then := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, then, then); err != nil {
		t.Fatal(err)
	}
	expectSync(t, bus, sources, 1)
	var session string
	if err := bus.db.QueryRow(`SELECT session_id FROM history WHERE id = 'u1'`).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if session != "first" {
		t.Fatalf("session = %q", session)
	}
}

func TestSyncUnreadableFileIsSkipped(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	writeRecords(t, filepath.Join(sources.Claude, "p", "good.jsonl"), prompt("u1", "", "one"))
	bad := filepath.Join(sources.Claude, "p", "bad.jsonl")
	writeRecords(t, bad, prompt("u2", "", "two"))
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(bad, 0o600)
	expectSync(t, bus, sources, 1)
}

func searchBus(t *testing.T) (string, *Bus, Sources) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aldan.sqlite3")
	bus, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bus.Close() })
	sources := newSources(t)
	writeRecords(t, filepath.Join(sources.Claude, "p", "s.jsonl"),
		prompt("u1", "", "Привет, Мир", rec{"timestamp": "2026-01-02T00:00:00Z"}),
		prompt("u2", "u1", "the zebra project", rec{"timestamp": "2026-01-01T00:00:00Z"}),
	)
	expectSync(t, bus, sources, 2)
	return path, bus, sources
}

func TestSearchRunsSQL(t *testing.T) {
	path, _, _ := searchBus(t)
	rows, err := search(path, `SELECT session_id, text, parent_id, 1 AS n FROM history WHERE text LIKE '%ZEBRA%'`)
	if err != nil {
		t.Fatal(err)
	}
	want := `[map[n:1 parent_id:u1 session_id:s text:the zebra project]]`
	if got := fmt.Sprint(rows); got != want {
		t.Fatalf("rows = %s", got)
	}
	rows, err = search(path, `SELECT text FROM history WHERE text LIKE '%Мир%'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	rows, err = search(path, `SELECT text FROM history WHERE text = 'absent'`)
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("rows = %#v", rows)
	}
	if _, err := search(path, `SELECT nope`); err == nil {
		t.Fatal("no error for bad SQL")
	}
}

func TestSearchIsReadOnly(t *testing.T) {
	path, bus, _ := searchBus(t)
	outside := filepath.Join(t.TempDir(), "outside.sqlite3")
	for _, query := range []string{
		`DELETE FROM history`,
		`PRAGMA query_only = OFF; DELETE FROM history`,
		`DROP TABLE history`,
		`INSERT INTO messages (id, "to", "from", prompt, meta, created_at) VALUES ('x', 'a', 'b', 'c', '{}', 'd')`,
		`ATTACH '` + outside + `' AS other; CREATE TABLE other.t (x)`,
		`ATTACH 'file:` + path + `?mode=rw' AS again; DELETE FROM again.history`,
	} {
		if _, err := search(path, query); err == nil {
			t.Fatalf("%s: no error", query)
		}
	}
	if got := historyCount(t, bus); got != 2 {
		t.Fatalf("history has %d rows", got)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("ATTACH created a file")
	}
}

func codexMeta(extra ...rec) rec {
	return rec{"type": "session_meta", "timestamp": "2026-01-01T00:00:00Z", "payload": merge(rec{
		"id": "c1", "cwd": "/w", "source": "cli", "originator": "codex-tui",
	}, extra...)}
}

func codexEvent(ts string, payload rec) rec {
	return rec{"type": "event_msg", "timestamp": ts, "payload": payload}
}

func codexItem(ts string, payload rec) rec {
	return rec{"type": "response_item", "timestamp": ts, "payload": payload}
}

func codexUser(ts, text string) rec {
	return codexEvent(ts, rec{"type": "user_message", "message": text})
}

func codexFinal(ts, text string) rec {
	return codexItem(ts, rec{"type": "message", "role": "assistant", "phase": "final_answer",
		"content": []any{rec{"type": "output_text", "text": text}}})
}

func TestCodexKeepsPromptsAndAnswers(t *testing.T) {
	rows := codexRows(t,
		codexMeta(),
		codexItem("t0", rec{"type": "message", "role": "user",
			"content": []any{rec{"type": "input_text", "text": "<environment_context>"}}}),
		codexUser("t1", "hello"),
		codexEvent("t2", rec{"type": "item_completed", "item": rec{"type": "UserMessage",
			"content": []any{rec{"type": "text", "text": "second form"}}}}),
		codexItem("t3", rec{"type": "message", "role": "assistant", "phase": "commentary",
			"content": []any{rec{"type": "output_text", "text": "looking"}}}),
		codexUser("t4", "   "),
		codexFinal("t5", "  "),
		codexFinal("t6", "done"),
	)
	expectTexts(t, rows, "hello", "second form", "done")
	user, final := rows[0], rows[2]
	if user.role != "user" || user.origin != "human" || user.session != "c1" || user.cwd != "/w" || user.ts != "t1" || user.agent != "codex" {
		t.Fatalf("user = %+v", user)
	}
	if final.role != "assistant" || final.origin != "" || final.parent != "" {
		t.Fatalf("final = %+v", final)
	}
	const id = "codex:"
	if !strings.HasPrefix(user.id, id) || len(user.id) != len(id)+64 {
		t.Fatalf("id = %q", user.id)
	}
}

func TestCodexSkipsProgramRuns(t *testing.T) {
	for name, meta := range map[string]rec{
		"plugin":   codexMeta(rec{"source": "vscode", "originator": "Claude Code"}),
		"exec":     codexMeta(rec{"source": "exec", "originator": "codex_exec"}),
		"subagent": codexMeta(rec{"source": rec{"subagent": "review"}}),
	} {
		t.Run(name, func(t *testing.T) {
			expectTexts(t, codexRows(t, meta, codexUser("t1", "hello")))
		})
	}
	expectTexts(t, codexRows(t, codexMeta(rec{"source": "vscode"}), codexUser("t1", "hello")), "hello")
}

func TestCodexNewMtimeAddsNothing(t *testing.T) {
	bus, sources := newBus(t), newSources(t)
	path := filepath.Join(sources.Codex, "2026", "01", "rollout.jsonl")
	writeRecords(t, path, codexMeta(), codexUser("t1", "hello"))
	expectSync(t, bus, sources, 1)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	expectSync(t, bus, sources, 0)
}

func TestCodexQuestionReply(t *testing.T) {
	const open, shut = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
	rows := codexRows(t, codexMeta(),
		codexUser("t1", open+`[{"id":"q1","answer":"yes"},{"id":"q2","answer":"no"}]`+shut),
		codexUser("t2", open+`not json`+shut),
		codexUser("t3", open+`[{"id":"q1"}]`+shut),
		codexUser("t4", "see "+open+`[{"answer":"x"}]`+shut),
	)
	expectTexts(t, rows, "yes\nno", open+`not json`+shut, open+`[{"id":"q1"}]`+shut, "see "+open+`[{"answer":"x"}]`+shut)
}

func busWithSession(t *testing.T, session string) *Bus {
	t.Helper()
	bus, sources := newBus(t), newSources(t)
	writeRecords(t, filepath.Join(sources.Claude, "p", "s.jsonl"), prompt("u-"+session, "", "hi", rec{"sessionId": session}))
	expectSync(t, bus, sources, 1)
	return bus
}

func metaOf(t *testing.T, message Message) map[string]any {
	t.Helper()
	meta := map[string]any{}
	if err := json.Unmarshal(message.Meta, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestSendDoesNotNeedTheIndex(t *testing.T) {
	bus := newBus(t)
	if _, err := bus.send("me", "anyone", "hi", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	messages, err := bus.inbox("anyone")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestInbox(t *testing.T) {
	bus := busWithSession(t, "peer")
	first, err := bus.send("me", "peer", "one", map[string]any{"task": "x", "session_id": "forged"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := bus.send("other", "peer", "two", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := bus.inbox("peer")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].ID != first || messages[1].ID != second {
		t.Fatalf("messages = %+v", messages)
	}
	if messages[0].From != "me" || messages[0].Prompt != "one" || messages[1].From != "other" {
		t.Fatalf("messages = %+v", messages)
	}
	meta := metaOf(t, messages[0])
	// meta is the sender's, untouched.
	want := map[string]any{"task": "x", "session_id": "forged"}
	if fmt.Sprint(meta) != fmt.Sprint(want) {
		t.Fatalf("meta = %v", meta)
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{}
	if err := json.Unmarshal(encoded, &item); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "from", "prompt", "meta", "created_at"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("no %s in %s", key, encoded)
		}
	}
	again, err := bus.inbox("peer")
	if err != nil {
		t.Fatal(err)
	}
	if again == nil || len(again) != 0 {
		t.Fatalf("second inbox = %#v", again)
	}
}

func TestTimestampsSortAsStrings(t *testing.T) {
	// Trailing zeros must stay: ".1Z" would sort after ".12Z".
	moment := time.Date(2026, 1, 1, 0, 0, 0, 100000000, time.UTC)
	if earlier, later := moment.Format(timeLayout), moment.Add(20*time.Millisecond).Format(timeLayout); earlier >= later {
		t.Fatalf("%s sorts after %s", earlier, later)
	}
}

func TestInboxReadersDoNotShareMessages(t *testing.T) {
	bus := busWithSession(t, "peer")
	const total = 20
	for i := 0; i < total; i++ {
		if _, err := bus.send("me", "peer", "hi", map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var got int
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			messages, err := bus.inbox("peer")
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			got += len(messages)
		}()
	}
	group.Wait()
	if got != total {
		t.Fatalf("readers got %d messages", got)
	}
}

func TestSelfID(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if _, err := selfID(); err == nil {
		t.Fatal("no error with no session id")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "claude")
	if id, _ := selfID(); id != "claude" {
		t.Fatalf("id = %q", id)
	}
	t.Setenv("CODEX_THREAD_ID", "codex")
	if id, _ := selfID(); id != "codex" {
		t.Fatalf("id = %q", id)
	}
}

func TestCommandsNeedASession(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	home := t.TempDir()
	for _, args := range [][]string{{"send", "a", "b"}, {"inbox"}} {
		if err := run(args, home, io.Discard); err == nil || !strings.Contains(err.Error(), "no session id") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
}

func TestCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ALDAN_DB", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "peer")
	writeRecords(t, filepath.Join(home, ".claude", "projects", "p", "s.jsonl"),
		prompt("u1", "", "the zebra <project>", rec{"sessionId": "peer"}))

	cli := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run(args, home, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got := cli("sync"); got != "added 1\n" {
		t.Fatalf("sync = %q", got)
	}
	if info, err := os.Stat(filepath.Join(home, ".aldan", "aldan.sqlite3")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("default database: %v %v", info, err)
	}
	if got := cli("search", `SELECT text FROM history WHERE text LIKE '%ZEBRA%'`); !strings.Contains(got, `"text": "the zebra <project>"`) {
		t.Fatalf("search = %q", got)
	}
	if got := cli("search", `SELECT text FROM history WHERE text = 'absent'`); got != "[]\n" {
		t.Fatalf("empty search = %q", got)
	}
	if got := cli("inbox"); got != "[]\n" {
		t.Fatalf("empty inbox = %q", got)
	}
	id := strings.TrimSpace(cli("send", "--meta", `{"big":12345678901234567890}`, "peer", "a <b> & c"))
	inbox := cli("inbox")
	for _, part := range []string{id, "a <b> & c", "12345678901234567890"} {
		if !strings.Contains(inbox, part) {
			t.Fatalf("inbox = %s", inbox)
		}
	}
	for _, meta := range []string{"null", "[1]", "{} x", "nope"} {
		if err := run([]string{"send", "--meta", meta, "peer", "hi"}, home, io.Discard); err == nil {
			t.Fatalf("--meta %s: no error", meta)
		}
	}
}

func TestReleDBOverride(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "other.sqlite3")
	t.Setenv("ALDAN_DB", path)
	if err := run([]string{"sync"}, home, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".aldan")); err == nil {
		t.Fatal("default directory was created")
	}
}

const rule = "Bash(aldan:*)"

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallKeepsExistingSettings(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	original := `{
  "model": "opus",
  "limit": 12345678901234567890,
  "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Bash(rm:*)"]},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "sync >/dev/null 2>&1"}]}],
    "SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "other"}]}]
  }
}
`
	writeFile(t, settings, original)
	codexHooks := filepath.Join(home, ".codex", "hooks.json")
	writeFile(t, codexHooks, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x","timeout":60}]}]}}`)
	toml := filepath.Join(home, ".codex", "config.toml")
	writeFile(t, toml, "model = \"gpt\"\n\n[tui]\ntheme = \"dark\"\n# [sandbox_workspace_write] was here once")

	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}

	text := readFile(t, settings)
	for _, part := range []string{`"model": "opus"`, `12345678901234567890`, `"Bash(ls:*)"`, `"Bash(rm:*)"`,
		`sync >/dev/null 2>&1`, `"matcher": "startup"`, `"other"`, rule, `"command": "aldan sync >/dev/null"`, `"async": true`} {
		if !strings.Contains(text, part) {
			t.Fatalf("settings lack %q:\n%s", part, text)
		}
	}
	if got := readFile(t, settings+".bak"); got != original {
		t.Fatalf(".bak is not the original:\n%s", got)
	}
	if text := readFile(t, codexHooks); !strings.Contains(text, `"command": "x"`) || strings.Count(text, `"timeout": 60`) != 2 || !strings.Contains(text, syncCommand) {
		t.Fatalf("codex hooks:\n%s", text)
	}
	want := "model = \"gpt\"\n\n[tui]\ntheme = \"dark\"\n# [sandbox_workspace_write] was here once\n[sandbox_workspace_write]\nwritable_roots = [\"" + filepath.Join(home, ".aldan") + "\"]\n"
	if got := readFile(t, toml); got != want {
		t.Fatalf("config.toml:\n%s", got)
	}
}

func TestInstallTwiceAddsOnce(t *testing.T) {
	home := t.TempDir()
	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".codex", "config.toml"),
	}
	before := map[string]string{}
	for _, path := range paths {
		before[path] = readFile(t, path)
		if _, err := os.Stat(path + ".bak"); err == nil {
			t.Fatalf("%s.bak exists for a new file", path)
		}
	}
	var out bytes.Buffer
	if err := install(home, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("second run reported: %s", out.String())
	}
	for _, path := range paths {
		if got := readFile(t, path); got != before[path] {
			t.Fatalf("%s changed:\n%s", path, got)
		}
		if _, err := os.Stat(path + ".bak"); err == nil {
			t.Fatalf("second run wrote %s.bak", path)
		}
	}
	claude, codex, toml := before[paths[0]], before[paths[1]], before[paths[2]]
	if strings.Count(claude, rule) != 1 || strings.Count(claude, syncCommand) != 1 ||
		strings.Count(codex, syncCommand) != 1 || strings.Count(toml, filepath.Join(home, ".aldan")) != 1 {
		t.Fatalf("duplicates:\n%s\n%s\n%s", claude, codex, toml)
	}
}

func TestInstallCreatesDataDirectory(t *testing.T) {
	home := t.TempDir()
	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, ".aldan"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v", info.Mode())
	}
}

func TestInstallStopsAtExistingSandboxSection(t *testing.T) {
	for name, existing := range map[string]string{
		"header": "[sandbox_workspace_write]\nnetwork_access = true\n",
		"dotted": "sandbox_workspace_write.network_access = true\n",
		"inline": "sandbox_workspace_write = { network_access = true }\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			toml := filepath.Join(home, ".codex", "config.toml")
			writeFile(t, toml, existing)
			err := install(home, io.Discard)
			root := filepath.Join(home, ".aldan")
			if err == nil || !strings.Contains(err.Error(), "[sandbox_workspace_write]") || !strings.Contains(err.Error(), root) {
				t.Fatalf("err = %v", err)
			}
			if got := readFile(t, toml); got != existing {
				t.Fatalf("config.toml changed:\n%s", got)
			}
		})
	}
}

func TestInstallAcceptsExistingRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".aldan")
	toml := filepath.Join(home, ".codex", "config.toml")
	existing := "[sandbox_workspace_write]\nwritable_roots = [\n  \"/other\",\n  \"" + root + "\",\n]\n"
	writeFile(t, toml, existing)
	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, toml); got != existing {
		t.Fatalf("config.toml changed:\n%s", got)
	}
}

func TestInstallRefusesToReplaceOddSettings(t *testing.T) {
	for name, existing := range map[string]string{
		"hooks":       `{"hooks": "keep me"}`,
		"Stop":        `{"hooks": {"Stop": "keep me"}}`,
		"permissions": `{"permissions": "keep me"}`,
		"allow":       `{"permissions": {"allow": "keep me"}}`,
		"invalid":     `{"keep me"`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			settings := filepath.Join(home, ".claude", "settings.json")
			writeFile(t, settings, existing)
			if err := install(home, io.Discard); err == nil {
				t.Fatal("no error")
			}
			if got := readFile(t, settings); got != existing {
				t.Fatalf("settings changed:\n%s", got)
			}
		})
	}
}

func TestInstallAddsAgentNote(t *testing.T) {
	home := t.TempDir()
	claude := filepath.Join(home, ".claude", "CLAUDE.md")
	writeFile(t, claude, "## Mine\n\nkeep this\n")
	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, claude), "## Mine\n\nkeep this\n\n"+agentNote; got != want {
		t.Fatalf("CLAUDE.md:\n%s", got)
	}
	if got := readFile(t, claude+".bak"); got != "## Mine\n\nkeep this\n" {
		t.Fatalf(".bak:\n%s", got)
	}
	if got := readFile(t, filepath.Join(home, ".codex", "AGENTS.md")); got != agentNote {
		t.Fatalf("AGENTS.md:\n%s", got)
	}
}

func TestUninstallRemovesWhatInstallAdded(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	codexHooks := filepath.Join(home, ".codex", "hooks.json")
	toml := filepath.Join(home, ".codex", "config.toml")
	claudeNote := filepath.Join(home, ".claude", "CLAUDE.md")
	originals := map[string]string{
		settings:   `{"model":"opus","permissions":{"allow":["Bash(ls:*)"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`,
		codexHooks: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x","timeout":60}]}]}}`,
		toml:       "model = \"gpt\"\n",
		claudeNote: "## Mine\n\nkeep this\n",
	}
	for path, text := range originals {
		writeFile(t, path, text)
	}
	if err := install(home, io.Discard); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "aldan")
	writeFile(t, binary, "binary")
	writeFile(t, filepath.Join(home, ".aldan", "aldan.sqlite3"), "data")
	if err := uninstall(home, binary, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{settings, codexHooks} {
		var got, want any
		if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(originals[path]), &want); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s = %v, want %v", path, got, want)
		}
	}
	for _, path := range []string{toml, claudeNote} {
		if got := readFile(t, path); got != originals[path] {
			t.Fatalf("%s:\n%q", path, got)
		}
	}
	if got := readFile(t, filepath.Join(home, ".codex", "AGENTS.md")); got != "" {
		t.Fatalf("AGENTS.md:\n%s", got)
	}
	for _, path := range []string{filepath.Join(home, ".aldan"), binary} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s is still there", path)
		}
	}
	var out bytes.Buffer
	if err := uninstall(home, binary, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "updated") {
		t.Fatalf("second uninstall changed files: %s", out.String())
	}
}

func TestUninstallLeavesTheUsersOwnRoot(t *testing.T) {
	home := t.TempDir()
	toml := filepath.Join(home, ".codex", "config.toml")
	existing := "[sandbox_workspace_write]\nwritable_roots = [\"" + filepath.Join(home, ".aldan") + "\"]\n"
	writeFile(t, toml, existing)
	var out bytes.Buffer
	if err := uninstall(home, filepath.Join(home, "aldan"), &out); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, toml); got != existing {
		t.Fatalf("config.toml changed:\n%s", got)
	}
	if !strings.Contains(out.String(), "yourself") {
		t.Fatalf("out = %s", out.String())
	}
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"-h"}, {"--help"}} {
		var out bytes.Buffer
		if err := run(args, t.TempDir(), &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out.String(), "aldan uninstall") {
			t.Fatalf("%v: %s", args, out.String())
		}
	}
	if err := run([]string{"nope"}, t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("err = %v", err)
	}
}
