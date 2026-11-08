*For agents: this spec is the source of truth. Where it disagrees with the code, the code is wrong. Don't edit the spec unless explicitly asked.*

`aldan` is a tiny utility layer for your local agents. For now, it does two things.

**Index.** It keeps a searchable copy of your conversation history in SQLite: what you asked and what the agents answered.
The index is brain-dead, local and best-effort. It holds only what you typed and the agents' final text answers. Empty messages, reasoning, tool calls, commentary and internal agent chats are left out. Some lag, and incomplete ancestry in old sessions, are acceptable. One reader of the saved transcripts serves both live updates and backfill, so hook payloads are not captured directly. Indexed data is never deleted. The index grows only when real use demands it: no speculative infrastructure, no extra services and no guessed ancestry.

How it works: a `Stop` hook updates the index from the session transcripts after every turn. A note in `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md` tells agents that the index exists, and they query it with read-only SQL through `aldan search`.


**Messages.** Agents can send messages to each other. With the index, you can tell an agent "do X, and notify the session where I did Y when done": the agent finds that session in the index and runs `aldan send`. You can also refer to a session by its id.
Messaging is local and best-effort. A session is just its id: there are no names, no registry and no liveness, so mail to a closed session waits until the session is resumed. `send` does not check the recipient. There is no server, MCP or daemon, only the CLI, the same SQLite file as the index, and `codex queue` for Codex sessions.

How it works: `aldan send` finds the agent of the recipient in the `agent` column of the index. If the session is not in the index yet, the id format tells which agent it is: Codex ids are UUID version 7, Claude ids are UUID version 4.

Codex: `aldan send` runs `codex queue`. Codex puts the message into the session as a user message, also when the session is idle. The text names the sender's session id. The aldan inbox is not used.

Claude: `aldan send` writes the message to the aldan inbox. `aldan watch` polls the inbox of the current session. When mail arrives, the watcher prints it and exits with code 2. A `SessionStart` hook and a `Stop` hook start the watcher, both with `asyncRewake`. Exit code 2 wakes the agent, also when it is idle. After each turn, the `Stop` hook starts a new watcher. Each session has only one watcher: the database keeps the process id of the watcher for each session id, and a new watcher stops the old one.

Message schema:

```json
{
  "id": "001ac1f43d97541e3ec8d18fbfd599ea",
  "from": "81e2754b-a65c-46da-ab11-d7e689a8786c",
  "hops": 0,
  "prompt": "...",
  "meta": {},
  "created_at": "2026-11-07T18:56:33.411160000Z"
}
```

Program iteslf is a Go CLI, installed on `PATH` as `aldan`. `aldan install` adds the hooks and the note, and `aldan uninstall` removes them, the database and the binary.

The note's text lives in `prompts.go`.
