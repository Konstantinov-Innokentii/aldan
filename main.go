// aldan lets Claude Code and Codex sessions on one Mac message each other through
// one SQLite file. The same file indexes past sessions, which is how an agent
// finds "the session where I did X".
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const usage = `usage:
  aldan send [--meta JSON] SESSION_ID PROMPT
  aldan inbox        print this session's mail and mark it delivered
  aldan watch        wait for this session's mail, print it to stderr and exit 2
  aldan search SQL   run read-only SQL against the index
  aldan sync         read new transcripts into the index
  aldan install      add the hooks, the permission, the Codex sandbox access and the agent note
  aldan uninstall    remove what install added, the database and the binary
  aldan help         print this help`

func main() {
	home, err := os.UserHomeDir()
	if err == nil {
		err = run(os.Args[1:], home, os.Stdout)
	}
	if errors.Is(err, errWake) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aldan:", err)
		os.Exit(1)
	}
}

// errWake makes aldan exit with code 2, which wakes a Claude Code session from
// an asyncRewake hook.
var errWake = errors.New("mail arrived")

func run(args []string, home string, out io.Writer) error {
	if len(args) == 0 {
		args = []string{"help"}
	}
	command, args := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(out, usage)
		return err
	case "install":
		return install(home, out)
	case "uninstall":
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		return uninstall(home, binary, out)
	case "send", "inbox", "watch", "search", "sync":
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}

	path := os.Getenv("ALDAN_DB")
	if path == "" {
		path = filepath.Join(home, ".aldan", "aldan.sqlite3")
	}
	sources := Sources{
		Claude: filepath.Join(home, ".claude", "projects"),
		Codex:  filepath.Join(home, ".codex", "sessions"),
	}

	switch command {
	case "send":
		return runSend(path, args, out)
	case "inbox":
		return runInbox(path, out)
	case "watch":
		return runWatch(path, os.Stderr)
	case "search":
		return runSearch(path, sources, args, out)
	}
	bus, err := open(path)
	if err != nil {
		return err
	}
	defer bus.Close()
	added, err := bus.sync(sources)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "added", added)
	return nil
}

func runSend(path string, args []string, out io.Writer) error {
	const sendUsage = "usage: aldan send [--meta JSON] SESSION_ID PROMPT"
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	metaJSON := flags.String("meta", "{}", "")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, sendUsage)
	}
	if flags.NArg() != 2 {
		return errors.New(sendUsage)
	}
	meta, err := decodeObject([]byte(*metaJSON))
	if err != nil {
		return fmt.Errorf("--meta must be a JSON object: %w", err)
	}
	self, err := selfID()
	if err != nil {
		return err
	}
	bus, err := open(path)
	if err != nil {
		return err
	}
	defer bus.Close()
	id, err := bus.send(self, flags.Arg(0), flags.Arg(1), meta)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, id)
	return nil
}

func runInbox(path string, out io.Writer) error {
	self, err := selfID()
	if err != nil {
		return err
	}
	bus, err := open(path)
	if err != nil {
		return err
	}
	defer bus.Close()
	messages, err := bus.inbox(self)
	if err != nil {
		return err
	}
	return printJSON(out, messages)
}

func runWatch(path string, out io.Writer) error {
	self, err := selfID()
	if err != nil {
		return err
	}
	bus, err := open(path)
	if err != nil {
		return err
	}
	defer bus.Close()
	messages, err := bus.watch(self, os.Getpid(), 3*time.Second)
	if err != nil || messages == nil {
		return err
	}
	if err := printJSON(out, messages); err != nil {
		return err
	}
	return errWake
}

func runSearch(path string, sources Sources, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: aldan search SQL")
	}
	bus, err := open(path)
	if err != nil {
		return err
	}
	defer bus.Close()
	if _, err := bus.sync(sources); err != nil {
		return err
	}
	rows, err := search(path, args[0])
	if err != nil {
		return err
	}
	return printJSON(out, rows)
}

func printJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

// selfID reads the session id that the harness gives every shell command. Codex
// goes first, because a Codex started from a Claude Code shell inherits Claude's
// variable.
func selfID() (string, error) {
	for _, name := range []string{"CODEX_THREAD_ID", "CLAUDE_CODE_SESSION_ID"} {
		if id := os.Getenv(name); id != "" {
			return id, nil
		}
	}
	return "", errors.New("no session id: run this from a Claude Code or Codex session")
}
