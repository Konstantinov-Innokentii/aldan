package main

// agentNote tells agents about the index and the messages.
const agentNote = `## Past conversations

My typed prompts and your final answers from Claude Code and Codex are indexed
by aldan. Search it only when I refer to earlier work ("remember when...", "what
did we decide about..."), ask for a retrospective, or ask you to message a
session without giving its id.

Query table ` + "`" + `history` + "`" + ` with read-only SQL:

    aldan search "SELECT ..."

Columns: ts, agent (claude/codex), role (user/assistant), text, session_id, cwd,
origin, parent_id. To read a whole conversation, select by session_id, ordered
by ts.

It holds no tool calls, reasoning or subagent chats, so it shows what was said,
not what was done. Search is literal: try several words, and try both Russian
and English.

## Messages between sessions

To message another session, run ` + "`" + `aldan send SESSION_ID TEXT` + "`" + `. If I describe the
session instead of giving its id, find the id with aldan search. Aldan records
your session id as the sender, so leave it out of TEXT.

The receiver knows nothing about your session, so TEXT must stand on its own:
say what you did or what you need, and include the result itself, not a pointer
to it. If you need to send structured data or metadata, put it in
` + "`" + `--meta '{...}'` + "`" + `, a JSON object passed through as is.
To answer a message, send to its ` + "`" + `from` + "`" + `.

A message from another session arrives on its own, as JSON with ` + "`" + `from` + "`" + `, ` + "`" + `prompt` + "`" + `
and ` + "`" + `meta` + "`" + `.

Keep all of this out of your replies. Don't mention how aldan works, session ids
or message ids. When a message arrives, say only "Received a new message." If it
asks you to do something, do it and answer as usual.
`
