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

   - `~/.claude/settings.json`: a `Stop` hook that runs `aldan sync`, a `SessionStart` and a `Stop` hook that run `aldan watch`, and the `Bash(aldan:*)` permission.
   - `~/.codex/hooks.json`: the same `Stop` hook.
   - `~/.codex/rules/aldan.rules`: a rule that lets `aldan send` run outside the sandbox, so it can hand messages to `codex queue`.
   - `~/.codex/config.toml`: write access to `~/.aldan`. If the file already has a `[sandbox_workspace_write]` section, the command stops and says what to add.
   - `~/.claude/CLAUDE.md` and `~/.codex/AGENTS.md`: a note that tells agents about the index and the messages.

3. Build the index of past sessions:

   ```bash
   aldan sync
   ```

4. Tell the user to restart their sessions, since hooks and the note load when a session starts. Codex can ask them to approve the new hook.

5. Tell the user how to check that it works. Open two sessions. In the first, type `I am working on the zebra project. Reply ok.` In the second, type:

   ```
   Work out 17*23, and notify the session where I worked on the zebra project with the result.
   ```

   The first session should wake up and report the message.

## Uninstall

```bash
aldan uninstall
```

This removes the hooks, the permission, the Codex rule and sandbox access, the note, the database in `~/.aldan` and the binary.
