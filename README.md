# aldan

`aldan` is a tiny utility layer for your local agents. For now, it does two things.

**Index.** It keeps a searchable copy of your conversation history in SQLite: what you asked and what the agents answered.

**Messages.** Agents can send messages to each other. With the index, you can tell an agent "do X, and notify the session where I did Y when done": the agent finds that session in the index and runs `aldan send`. You can also refer to a session by its id.

## Install

> [!TIP]
> Send this page's link to Claude or Codex and ask it to install aldan.

Requires Go 1.26+.

1. Clone the repo and build the binary into a directory on `PATH`. The hooks and the note call it as `aldan`:

   ```bash
   git clone https://github.com/Konstantinov-Innokentii/aldan.git
   cd aldan
   mkdir -p ~/.local/bin
   go build -o ~/.local/bin/aldan .
   ```

2. Add the hooks and permissions:

   ```bash
   aldan install
   ```

   It changes these files, keeping a `.bak` copy of each:

   - `~/.claude/settings.json`: a `Stop` hook that runs `aldan sync`, and the `Bash(aldan:*)` permission.
   - `~/.codex/hooks.json`: the same `Stop` hook.
   - `~/.codex/config.toml`: write access to `~/.aldan`. If the file already has a `[sandbox_workspace_write]` section, the command stops and says what to add.
   - `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`: a note that tells agents about the index and how to poll for messages.

3. Build the index of past sessions:

   ```bash
   aldan sync
   ```

   This takes about 10 seconds for 10,000 messages.

4. Tell the user to restart their sessions, since hooks and the note load when a session starts. Codex can ask them to approve the new hook.

5. Tell the user how to check that it works. Open two sessions. In the first, type `I am working on the zebra project. Reply ok.` In the second, type:

   ```
   Work out 17*23, and notify the session where I worked on the zebra project with the result.
   ```

   The first session should wake up and report the message.

## Use

Tell any session what to do in plain words:

- `Run the tests, and tell the session where I worked on the auth refactor when done.`
- `Ask the session about the Stripe webhook what it found.`

## Uninstall

```bash
aldan uninstall
```

This removes the hooks, the permission, the Codex sandbox access, the note, the database in `~/.aldan` and the binary.
