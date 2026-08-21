package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"agentforge/internal/message"
	"agentforge/internal/schema"
)

// outputOptions controls how a driven run's outcome is rendered. The zero
// value reproduces the CLI's original behavior exactly (a human-readable
// trace on stdout), so every pre-existing call site that doesn't thread
// one through keeps working unchanged.
type outputOptions struct {
	format string // "" or "text" | "json"
	path   string // "" = stdout
}

func (o outputOptions) json() bool { return o.format == "json" }

func (o outputOptions) validate() error {
	switch o.format {
	case "", "text", "json":
		return nil
	default:
		return fmt.Errorf("--output-format must be text or json, got %q", o.format)
	}
}

// addOutputFlags registers --output-format and --output on any command
// that drives a run to an outcome: run, and runs resume/approve/deny. Not
// runs list/get — those inspect a run rather than produce an outcome to
// pipe, and printRunTrace/printRunsList already serve that job.
func addOutputFlags(cmd *cobra.Command, o *outputOptions) {
	cmd.Flags().StringVar(&o.format, "output-format", "text", "outcome format: text | json")
	cmd.Flags().StringVar(&o.path, "output", "", "write the run outcome here instead of stdout")
}

// resolveMessage returns m unchanged, unless it starts with "@", in which
// case the rest is a path whose contents (verbatim — no trailing-newline
// trim) become the message. A missing/unreadable file is a hard error
// naming the path: a silent fallback to treating "@foo.txt" as a literal
// message would hide a typo'd path instead of failing fast.
func resolveMessage(m string) (string, error) {
	path, ok := strings.CutPrefix(m, "@")
	if !ok {
		return m, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read message file %s: %w", path, err)
	}
	return string(b), nil
}

// progressWriter is where incidental progress lines ("run … started", the
// approval hint) go. Text mode leaves them on stdout, unchanged from
// before this file existed. JSON mode moves them to stderr so stdout
// carries only the envelope and can be piped straight into jq.
func progressWriter(o outputOptions) io.Writer {
	if o.json() {
		return os.Stderr
	}
	return os.Stdout
}

// outputTarget resolves where a run outcome's payload (the text trace or
// the JSON envelope) is written, and returns a closer — a no-op for
// stdout, os.File.Close for --output PATH. PATH is created (0644,
// truncating) rather than appended to; a missing parent directory is
// reported as an error, not silently created.
func outputTarget(o outputOptions) (io.Writer, func() error, error) {
	if o.path == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(o.path)
	if err != nil {
		return nil, nil, fmt.Errorf("open --output %s: %w", o.path, err)
	}
	return f, f.Close, nil
}

// runResult is the local/remote-common shape of a driven run's outcome —
// the third member of the family print.go already established with
// toolCallRow and runRow. driveLocalRun and the remote run/resume/approve
// paths each populate one, so emitRunResult never needs to know which
// mode produced it.
type runResult struct {
	RunID      string
	State      string
	Error      *string
	Messages   []message.Message
	Pending    []remotePendingCall
	DurationMS int64
	SchemaSet  bool // agent config has output.schema set (gates JSON-typed `output`)

	// Server is set only for the remote path; it's what puts " --server
	// <url>" on the awaiting_approval hint in text mode. Empty for local.
	Server string
}

// emitRunResult writes res to w in the requested format. Text format is
// byte-identical to what this codebase printed before --output-format
// existed (printMessages / the hand-written awaiting_approval block) —
// existing callers must see no difference.
func emitRunResult(w io.Writer, o outputOptions, res runResult) error {
	if o.json() {
		return emitRunResultJSON(w, res)
	}
	emitRunResultText(w, res)
	return nil
}

func emitRunResultText(w io.Writer, res runResult) {
	if res.State == "awaiting_approval" {
		fmt.Fprintf(w, "run %s is awaiting approval:\n", res.RunID)
		for _, p := range res.Pending {
			fmt.Fprintf(w, "  %s  %s(%s)\n", p.CallID, p.Tool, p.Args)
		}
		hint := fmt.Sprintf("decide with: agentforge runs approve|deny %s <call-id>", res.RunID)
		if res.Server != "" {
			hint += " --server " + res.Server
		}
		fmt.Fprintln(w, hint)
		return
	}
	printMessages(w, res.Messages)
}

// runEnvelope is the JSON shape emitted by --output-format json. Field
// names deliberately diverge from the AlgoMotion roadmap's sketch in two
// places, each recorded because it's the kind of thing that looks like a
// typo later: "state" (not "status") matches every other surface in this
// codebase — store.Run.State, the HTTP API, `runs list`; and "tokens" is
// omitted entirely rather than emitted as 0, because nothing in the
// runtime records token usage yet (that's a later roadmap item) and a
// fake 0 is a lie scripts would start depending on.
type runEnvelope struct {
	RunID          string              `json:"run_id"`
	State          string              `json:"state"`
	Error          *string             `json:"error,omitempty"`
	Output         json.RawMessage     `json:"output,omitempty"`
	ToolCallsCount int                 `json:"tool_calls_count"`
	DurationMS     int64               `json:"duration_ms"`
	Pending        []remotePendingCall `json:"pending,omitempty"`
}

func emitRunResultJSON(w io.Writer, res runResult) error {
	env := runEnvelope{
		RunID:          res.RunID,
		State:          res.State,
		Error:          res.Error,
		ToolCallsCount: countToolUses(res.Messages),
		DurationMS:     res.DurationMS,
		Output:         finalOutputJSON(res),
	}
	if res.State == "awaiting_approval" {
		env.Pending = res.Pending
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(env)
}

// countToolUses counts BlockToolUse blocks across every message in the
// run, local or remote — both always have the full message list, so this
// needs no store query and no internal/api change for parity.
func countToolUses(msgs []message.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == message.BlockToolUse {
				n++
			}
		}
	}
	return n
}

// finalAssistantText concatenates the text blocks of the last assistant
// message in msgs. ok is false when there is no assistant message, or the
// last one has no text (e.g. it ends in a pending tool call) — the caller
// omits `output` in that case rather than emitting an empty string.
func finalAssistantText(msgs []message.Message) (text string, ok bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.RoleAssistant {
			continue
		}
		var sb strings.Builder
		found := false
		for _, b := range msgs[i].Content {
			if b.Type == message.BlockText {
				sb.WriteString(b.Text)
				found = true
			}
		}
		return sb.String(), found
	}
	return "", false
}

// finalOutputJSON builds the envelope's `output` field. It's a nested
// JSON value, not a string, only when the run completed and the agent's
// config set output.schema — item 2 already guarantees a completed run's
// final answer parses and validates against that schema, so extraction
// here cannot fail in practice. Every other case (no schema, or a schema
// but a non-completed state) emits a plain JSON string: gating the type
// on config rather than "does this text look like JSON" keeps the type
// stable per agent, so a prose agent that happens to mention {"a":1}
// mid-sentence never gets silently truncated to that fragment.
func finalOutputJSON(res runResult) json.RawMessage {
	text, ok := finalAssistantText(res.Messages)
	if !ok {
		return nil
	}
	if res.State == "completed" && res.SchemaSet {
		if raw, err := schema.ExtractJSON(text); err == nil {
			return json.RawMessage(raw)
		}
		// Falls through to the string form below. Shouldn't happen for a
		// completed run — defensive, not a path this codebase's own runs
		// are expected to take.
	}
	b, err := json.Marshal(text)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}
