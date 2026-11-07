# aldan

`aldan` is a tiny utility layer for your local agents. For now, it does two things.

**Index.** It keeps a searchable copy of your conversation history in SQLite: what you asked and what the agents answered.

**Messages.** Agents can send messages to each other. With the index, you can tell an agent "do X, and notify the session where I did Y when done": the agent finds that session in the index and runs `aldan send`. You can also refer to a session by its id.

## Install

You need Go 1.26 or later, and Claude Code or Codex.

1. Build the binary into a directory on your `PATH`. The hooks and the note call it as `aldan`:

   ```bash
   mkdir -p ~/.local/bin
   go build -o ~/.local/bin/aldan .
   ```

2. Add the hooks and permissions:

   ```bash
   aldan install
   ```

   This adds a `Stop` hook that runs `aldan sync` to `~/.claude/settings.json` and `~/.codex/hooks.json`, the `Bash(aldan:*)` permission, and Codex write access to `~/.aldan` in `~/.codex/config.toml`. It also appends a note to `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md` that tells agents about the index and how to poll for messages. Each changed file keeps a `.bak` copy. If your Codex config already has a `[sandbox_workspace_write]` section, the command stops and tells you what to add.

3. Build the index of your past sessions:

   ```bash
   aldan sync
   ```

   This takes about 10 seconds for 10,000 messages.

4. Restart your sessions. Hooks and the note load when a session starts. Codex can ask you to approve the new hook.

## Check that it works

```bash
aldan search "SELECT ts, session_id, text FROM history WHERE text LIKE '%a word from a past session%'"
```

You should see matching messages as JSON, with their session ids.

Then open two sessions. In the first, type `I am working on the zebra project. Reply ok.` In the second, type:

```
Work out 17*23, and notify the session where I worked on the zebra project with the result.
```

The first session should wake up and report the message.

## Use

Tell any session what to do in plain words:

- `Run the tests, and tell the session where I worked on the auth refactor when done.`
- `Ask the session about the Stripe webhook what it found.`

You do not run the commands yourself.

You can also query the index from your terminal, with any read-only SQL:

```bash
aldan search "SELECT ts, agent, role, session_id, text FROM history WHERE text LIKE '%stripe webhook%' ORDER BY ts"
```

## Good to know

- **Claude Code** runs its watcher in the background, so you can keep using the session.
- **Codex** runs its watcher inside its turn. Interrupt it to type. Long waits cost model calls, because Codex keeps checking the running command.
- **Closed sessions** keep their mail until you resume them.
- **Scripted runs** (`claude -p`, `codex exec`) can send mail but cannot receive it.

## Data

Everything is in `~/.aldan/aldan.sqlite3`, readable only by you. The index holds your typed prompts and the agents' final answers, which can include secrets.

## Uninstall

```bash
aldan uninstall
```

This removes the hooks, the permission, the Codex sandbox access, the note, the database in `~/.aldan` and the binary.
