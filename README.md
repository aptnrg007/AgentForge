# AgentForge

A single-binary runtime that turns a YAML file into a running AI agent with MCP tools, an HTTP API, and human approval gates — on your own machine, no cloud account required.

## Quickstart

```
ollama pull qwen2.5-coder:14b
ollama serve

go build -o agentforge ./cmd/agentforge
./agentforge run examples/minimal.yaml -m "what tools do you have?"
```

No signup, no API key, nothing to configure — the config file *is* the agent.

## Why

Every run is a persisted state machine, not a `for` loop: state, messages, and every tool call live in SQLite from the first line of code. That's what makes the rest of this possible without a rewrite —

- **Resumable.** Kill the process mid-run and it picks back up exactly where it left off.
- **Inspectable.** `agentforge runs get <id>` shows the full trace: every message, every tool call, who approved it, what came back.
- **Gated.** Put a tool behind `approvals.require` and the run pauses until a human says yes — denial doesn't kill the run, it feeds an error back to the model so it can try something else.

```
$ agentforge chat examples/everything-demo.yaml
chatting with everything-demo (ctrl+c to quit)
you: what's the sum of 2 and 40?
agent: calling everything.get-sum({"a":2,"b":40})

approval needed (1/1): everything.get-sum
  args: {"a":2,"b":40}
[y]es  [n]o  [e]dit  [a]pprove all remaining
```

`e` lets you fix sloppy arguments before they run — the thing that makes a 14B local model actually survivable as a tool caller.

## Agent config

```yaml
name: github-assistant

model:
  provider: ollama              # ollama | anthropic | openai
  name: qwen2.5-coder:14b
  temperature: 0.2
  # api_key: ${ANTHROPIC_API_KEY}   # anthropic/openai; same ${VAR} resolution as GITHUB_TOKEN below
  # base_url: https://api.groq.com/openai/v1   # openai only — any OpenAI-compatible endpoint
  #                                             # (Groq, Together, xAI/Grok, vLLM, llama.cpp, ...); api_key
  #                                             # is only required when base_url is left unset

instructions: |
  You are a GitHub assistant. Use the available tools to answer questions
  about repositories.

mcp:
  - name: github
    transport: stdio
    command: ["npx", "-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_TOKEN: ${GITHUB_TOKEN}   # resolved from the environment at load time

tools:
  - "github.*"                  # namespaced glob filter over the MCP tool list

approvals:
  mode: annotated                # never | annotated | always
  require:
    - "github.create_issue"
  auto_approve:
    - "github.get_*"
  timeout: 30m
  on_timeout: deny

# output:                         # optional: validate the final answer against a JSON Schema
#   schema: ./schemas/issue-list.json
#   on_invalid: retry              # retry | fail
#   max_retries: 2

# tool_policy:                     # optional: bound how long a single tool call may run
#   timeout: 30s                   # default for every tool; absent = unbounded (today's behavior)
#   on_timeout: error              # error (default, feed it back to the model) | fail (end the run)
#   overrides:                     # ordered — first matching pattern wins
#     - tools: ["github.*"]
#       timeout: 90s

limits:
  max_turns: 10
  max_tokens: 4096
```

Unknown keys are load errors, not silently ignored typos. A missing `${GITHUB_TOKEN}` (or `${ANTHROPIC_API_KEY}`) fails at load time with a clear message, not the first time a tool tries to use it — and `provider: anthropic`/`provider: openai` without an `api_key` fails the same way, before any request goes out (an `openai` config with `base_url` set is the one exception — see `examples/openai.yaml`). See `examples/minimal.yaml` and `examples/everything-demo.yaml` for working configs — the latter uses the public `@modelcontextprotocol/server-everything` reference server, so it runs with zero credentials. Point any of them at Anthropic or OpenAI instead of Ollama by swapping `model.provider`/`model.name`/`model.api_key`; nothing else in the config changes.

## Real agents

Two configs against real, credential-free MCP servers, for actually using this rather than kicking the tires:

```
export AGENTFORGE_FS_ROOT=$(pwd)
./agentforge chat examples/filesystem-assistant.yaml   # @modelcontextprotocol/server-filesystem, writes gated behind approval

export AGENTFORGE_MEMORY_PATH=/tmp/notes.json
./agentforge run examples/notes-assistant.yaml -m "remember that I like tea"   # @modelcontextprotocol/server-memory, ungated
```

The two make a deliberate contrast: the filesystem agent gates every mutating tool (`write_file`, `edit_file`, `move_file`, `create_directory`) behind `approvals.require`; the notes agent has no `approvals` section at all, because gating writes to a local knowledge-graph file would just be friction.

## Structured output

Point `output.schema` at a JSON Schema file and a run's final answer (not tool calls — a
turn that calls a tool is never validated) has to conform to it before the run completes:

```
./agentforge run examples/structured-output.yaml -m "a story about a lighthouse keeper"
```

If the model's answer violates the schema, the error is fed back as a normal user turn and
the model gets another shot — the same self-correction loop already used for a malformed
tool call, sharing its retry counter (see `runs get`, which shows both kinds of retry in
the trace by their message shape). Three numbers interact:

| | scope | default |
|---|---|---|
| `limits.max_turns` | every model call in the run | 10 |
| `output.max_retries` | consecutive schema violations | 2 |
| (tool-call repair, not configurable) | consecutive malformed tool calls | 2 |

Every retry burns a turn, so `max_turns` is the hard ceiling regardless of how generous
`max_retries` is — a run that keeps violating its schema past `max_turns` fails with "max
turns exceeded," not a schema error. `max_retries: 0` behaves like `on_invalid: fail`: one
attempt, no second chance.

Native enforcement varies by provider: Ollama enforces the schema (via `format`) only when
the agent has no tools registered — constrained decoding makes tool calls impossible on
that turn, so a tool-using agent on Ollama always takes the fallback below, same as
Anthropic (no native support at all yet). OpenAI is the exception: `response_format`
composes with `tools` on the same request, so an OpenAI-backed agent gets native
enforcement *and* tool use together — the one combination nothing else here can do yet.
Wherever native enforcement doesn't apply, validation happens by inspecting the model's
text after the fact instead — same guarantee, one extra round trip on a violation.

**Structured output only gates one-shot runs** (`run`, the HTTP API) — `chat` never
validates, since forcing every conversational reply to conform to a schema would make an
interactive session unusable.

A relative `output.schema` path resolves against the config file's own directory — but
only when the config was loaded from a file directly (`run`, `chat`). A run resumed from
the daemon or `runs approve`/`resume` reconstructs its config from the copy stored in
SQLite, which has no source directory; use an absolute path if the agent might run that
way. The error message says so if you get it wrong.

## Tool timeouts

With no `tool_policy` block, a tool call has no deadline of its own — a hung MCP server (a
stdio child that accepts a request and never answers) wedges the run forever, with no
config knob to bound it. `tool_policy` closes that gap:

```yaml
tool_policy:
  timeout: 30s          # default applied to every tool call
  on_timeout: error      # error (default) | fail
  overrides:              # ordered — first matching pattern wins
    - tools: ["github.*"]
      timeout: 90s
    - tools: ["render.video", "build.*"]
      timeout: 10m
```

`on_timeout: error` (the default) feeds the timeout back to the model as a tool-result
error, exactly like a denied call — the run survives and the agent gets to try something
else. `on_timeout: fail` ends the run instead, the same as an exhausted schema-retry
budget. There is no implicit default: a config with no `tool_policy` at all keeps every
tool unbounded, unchanged from before this existed — opting in is explicit.

A timed-out call also forces the MCP server it belongs to to reconnect on its next use
(`internal/mcp` drops any session that returns an error, deadline included — abandoning a
request mid-flight would desync a stdio JSON-RPC stream), so a tool that repeatedly times
out restarts its server each time.

`limits.timeout` is parsed and validated but not yet enforced — a separate, still-open gap.

## CLI

```
agentforge run <agent.yaml> -m "<message>" [--server URL]        # one-shot; embedded engine, or a running daemon
agentforge chat <agent.yaml>                                      # interactive REPL with approval prompts
agentforge serve [--addr 127.0.0.1:8080]                          # the HTTP daemon
agentforge agents list|get|delete [--server URL]                   # local store or daemon
agentforge runs list [--agent NAME] [--limit N] [--server URL]     # most recent first
agentforge runs get <id> [--server URL]                            # full trace: messages, tool calls, who approved
agentforge runs approve|deny <id> <call-id> [--reason TEXT]        # decide a pending call and continue the run
agentforge runs resume <id> [--server URL]                         # continue a run whose calls are already decided
```

`run` and the `agents`/`runs` commands work standalone against a local SQLite file (`~/.agentforge/agentforge.db` by default, override with `--db`), or against a running daemon via `--server` — same config, same behavior, either way. `agentforge run` exits non-zero when the run fails or hits an unhandled error, so it's safe to chain in a script; a run that pauses for approval prints the pending call IDs and exits 0 — decide with `runs approve`/`deny`.

`run`, `runs approve`, `runs deny`, and `runs resume` all take `-m`/`--message` as `@path`
to read it from a file instead of the command line (`run script-agent.yaml -m
@beat-sheet.txt` — `-m` itself is `run`-only, since the others are just continuing an
in-progress run), and `--output-format json --output PATH` to write a
`{run_id, state, output, tool_calls_count, duration_ms}` envelope instead of the usual
human-readable trace — `output` is a nested JSON value when the agent's `output.schema` is
set, a plain string otherwise:

```
./agentforge run examples/structured-output.yaml -m @prompt.txt --output-format json --output spec.json
jq .output.beats spec.json
```

In JSON mode, progress lines move to stderr so stdout carries only the envelope and can be
piped straight into `jq`; in text mode (the default) nothing changes.

## HTTP API

`agentforge serve` binds `127.0.0.1:8080` by default. There's no auth in v0.1 — keep it on localhost.

```
POST   /v1/agents                    # create/update from a YAML body
GET    /v1/agents                    GET /v1/agents/{name}    DELETE /v1/agents/{name}
GET    /v1/agents/{name}/tools       # resolved, filtered, namespaced tool list

POST   /v1/agents/{name}/run         # 200 with a result, or 202 with pending approvals
POST   /v1/agents/{name}/stream      # same, but Server-Sent Events: token/tool_call/tool_result as they happen
GET    /v1/runs                      # most recent first; ?agent=name and ?limit=n
GET    /v1/runs/{id}                 # full trace
POST   /v1/runs/{id}/approve         # {call_id, decision: "approved"|"denied", reason?}
POST   /v1/runs/{id}/resume          # drive the run forward after approving/denying

GET    /healthz
```

## What's here (and what isn't)

Built so far: the persisted run state machine with tool-call repair, an MCP client with process supervision and crash recovery, YAML config with env interpolation, the HTTP daemon, the full CLI (including driving a run through an approval gate and back, and listing runs, from the command line — not just from `chat`), approval gates with timeouts, a chat REPL for driving all of it interactively, SSE streaming on `/v1/agents/{name}/stream`, three providers behind one `Provider` interface — Ollama, Anthropic, and OpenAI (plus anything OpenAI-compatible via `base_url`: Groq, Together, xAI/Grok, vLLM, llama.cpp, ...) — with the same approval/denial/resume flow regardless of which; schema-validated structured output (`output.schema`) with automatic self-correction, native alongside tool use on OpenAI, native with no tools on Ollama, a validate-and-retry fallback everywhere else; and structured run output (`--output-format json`, `--output PATH`, `-m @file`) for scripting `run`/`runs approve|deny|resume`.

Not yet: streaming isn't wired into the CLI (`run`/`chat` still get one atomic result), native structured output on Anthropic (forced tool-use — fallback validation works today, just costs an extra round trip on a violation), OpenAI's `strict:true` schema mode (would need a conformance check against the schema subset it requires), per-tool timeouts, and everything explicitly deferred — dashboard, Kubernetes, multi-tenancy, Postgres/Redis, RAG, multi-agent workflows, a plugin SDK (MCP *is* the plugin system), and a visual builder. None of that is missing by accident.

## Building

```
CGO_ENABLED=0 go build -o agentforge ./cmd/agentforge
```

Pure Go throughout (including SQLite, via `modernc.org/sqlite`) — no cgo, so this produces a genuinely static, single-file binary that runs on a machine with no Go toolchain installed.
