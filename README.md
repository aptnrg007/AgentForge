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
  provider: ollama              # ollama | anthropic (openai comes later)
  name: qwen2.5-coder:14b
  temperature: 0.2
  # api_key: ${ANTHROPIC_API_KEY}   # anthropic only; required, same ${VAR} resolution as GITHUB_TOKEN below

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

limits:
  max_turns: 10
  max_tokens: 4096
```

Unknown keys are load errors, not silently ignored typos. A missing `${GITHUB_TOKEN}` (or `${ANTHROPIC_API_KEY}`) fails at load time with a clear message, not the first time a tool tries to use it — and `provider: anthropic` without an `api_key` at all fails the same way, before any request goes out. See `examples/minimal.yaml` and `examples/everything-demo.yaml` for working configs — the latter uses the public `@modelcontextprotocol/server-everything` reference server, so it runs with zero credentials. Point either at Anthropic instead of Ollama by swapping `model.provider`/`model.name`/`model.api_key`; nothing else in the config changes.

## Real agents

Two configs against real, credential-free MCP servers, for actually using this rather than kicking the tires:

```
export AGENTFORGE_FS_ROOT=$(pwd)
./agentforge chat examples/filesystem-assistant.yaml   # @modelcontextprotocol/server-filesystem, writes gated behind approval

export AGENTFORGE_MEMORY_PATH=/tmp/notes.json
./agentforge run examples/notes-assistant.yaml -m "remember that I like tea"   # @modelcontextprotocol/server-memory, ungated
```

The two make a deliberate contrast: the filesystem agent gates every mutating tool (`write_file`, `edit_file`, `move_file`, `create_directory`) behind `approvals.require`; the notes agent has no `approvals` section at all, because gating writes to a local knowledge-graph file would just be friction.

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

Built so far: the persisted run state machine with tool-call repair, an MCP client with process supervision and crash recovery, YAML config with env interpolation, the HTTP daemon, the full CLI (including driving a run through an approval gate and back, and listing runs, from the command line — not just from `chat`), approval gates with timeouts, a chat REPL for driving all of it interactively, SSE streaming on `/v1/agents/{name}/stream`, and an Anthropic provider alongside Ollama — same `Provider` interface, same approval/denial/resume flow, `model.provider: anthropic` is the only config change.

Not yet: streaming isn't wired into the CLI (`run`/`chat` still get one atomic result), an OpenAI provider, structured output / schema validation, per-tool timeouts, and everything explicitly deferred — dashboard, Kubernetes, multi-tenancy, Postgres/Redis, RAG, multi-agent workflows, a plugin SDK (MCP *is* the plugin system), and a visual builder. None of that is missing by accident.

## Building

```
CGO_ENABLED=0 go build -o agentforge ./cmd/agentforge
```

Pure Go throughout (including SQLite, via `modernc.org/sqlite`) — no cgo, so this produces a genuinely static, single-file binary that runs on a machine with no Go toolchain installed.
