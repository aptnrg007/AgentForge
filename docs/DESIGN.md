# AgentForge design notes

Several doc comments in this codebase point at `PLAN.md` — a design document from
early development that was never committed to this repository. Rather than either
deleting those references or inventing a fake history, this file replaces it: same
section numbers the comments already cite, describing the system as it exists today.

## Ground rules

The codebase consistently follows five conventions. They aren't enforced by a linter;
they're conventions a reviewer can check a change against.

1. **Persisted state machine.** Nothing about a run lives only in memory. Every
   `Engine.Step` call loads state from SQLite, performs exactly one transition, and
   writes it back — see §5. This is what makes resume-after-restart free instead of a
   separate feature.
2. **No shell, ever.** A `command:`-backed tool's `argv` is passed to `exec.Command`
   as discrete arguments — never through `/bin/sh -c`. A rendered template value
   containing `;`, spaces, or `$(...)` arrives as one inert argument. See
   `internal/tools/command.go`.
3. **Fail at load time, not at use time.** A missing `${ENV_VAR}`, an invalid schema,
   an unresolvable `api_key` — all of these are load errors. The goal is that a
   misconfigured agent never gets partway into a run before the problem surfaces.
4. **Nothing runs unattended without an explicit policy decision.** Every tool call
   is evaluated against `approvals` before it executes (§5), and command-backed tools
   default to requiring approval even under `approvals.mode: never`, because handing
   a model unguarded process execution isn't a safe default to fall into silently.
5. **Unknown keys are load errors, not silently ignored typos.** Config parsing uses
   `yaml.UnmarshalStrict`; a stray or misspelled key fails the same way a missing
   required field would.

## §4 — YAML agent config schema

`internal/config/schema.go` defines the structs; loading is YAML → JSON (via
`sigs.k8s.io/yaml`) → strict struct decode (`internal/config/load.go`). `${VAR}`
references are resolved by reflecting over every string field (`internal/config/env.go`)
so a typo'd secret fails at load, matching ground rule 3.

Top-level keys: `name`, `model`, `instructions`, `mcp`, `tool_definitions`, `tools`,
`approvals`, `limits`, `session`, `output`, `tool_policy`. See `README.md`'s "Agent
config" section for the annotated example; `internal/config/validate.go` is the
authoritative source for cross-field rules (duplicate namespaces, literal-host
requirements on templated URLs, provider-specific `api_key` requirements, and so on).

## §5 — The run state machine

`internal/runtime/runtime.go` is the core. States: `ready_for_model`,
`awaiting_approval`, `ready_for_tools`, `completed`, `failed`, `cancelled`.
`Engine.Step` dispatches to `stepModel`, `stepTools`, or `stepAwaitingApproval` based
on the run's persisted state, and every path ends in exactly one write to the `runs`
table (ground rule 1).

- `stepModel` calls the provider, persists the assistant turn, validates any tool
  calls it made (malformed calls trigger a bounded self-correction loop — see
  `maxRepairAttempts`), evaluates the approval policy per call, and transitions to
  `ready_for_tools` or `awaiting_approval`.
- `stepTools` executes every settled call (respecting `tool_policy` timeouts),
  persists results, and returns to `ready_for_model`.
- `stepAwaitingApproval` is lazy: there is no background timer polling for an
  elapsed approval timeout. Whoever next calls `Step` (a CLI command, an API request)
  discovers the timeout and resolves it, consistent with ground rule 1 — state is
  re-evaluated from persisted data, not ticked by a goroutine.

Callers loop on `Step` until it returns a terminal state or `awaiting_approval`; see
`internal/cli/localrun.go`, `internal/api/handlers.go`, and `internal/api/stream.go`
for the three current drive loops.

## §8 — MCP process supervision and namespacing

`internal/mcp/server.go` and `registry.go`. One long-lived subprocess per **unique
config** — keyed by a hash of command + sorted env, not by the `mcp:` entry's name —
so two YAML entries with identical configs share one process instead of spawning a
redundant duplicate. Connection is lazy (first tool call, not agent load) with
exponential backoff on failure.

Tool names are namespaced as `<mcp-entry-name>.<bare-tool-name>` (e.g.
`github.search`) before the model ever sees them. The namespace comes from the
config's `mcp[].name`, not the server's own identity, so the same physical server
connected twice under different names produces two independent tool namespaces with
nothing to reconcile.

## §9 — HTTP API

`internal/api`. No auth in v0.1 — the daemon is meant for `127.0.0.1` only; adding
auth before the daemon can safely leave localhost is a tracked follow-up. Routes
create/list/inspect agents and runs, drive a
run to its next stop point (`POST .../run`), stream progress over SSE
(`POST .../stream`), and resolve pending approvals (`POST /v1/runs/{id}/approve`).
Every handler reconstructs its `runtime.Engine` from the run's persisted YAML
(`buildEngineForRun`) rather than holding one in memory, again per ground rule 1.

## §11 — Testing approach

The dominant pattern is **fixture replay**: a scripted fake implementing
`provider.Provider` (`fakeProvider` in `internal/runtime/runtime_test.go`) returns a
pre-defined sequence of responses, so state-machine behavior — repair loops,
approval evaluation, timeout handling — is tested deterministically and fast,
without a live model. The two "integration" tests
(`internal/runtime/anthropic_integration_test.go`,
`internal/runtime/gemini_integration_test.go`) apply the same idea one level down:
they drive a real `provider.Anthropic`/`provider.Gemini` against an `httptest.Server`
emulating the real wire protocol, rather than the fake `Provider` interface, so a
message-translation bug on a resumed turn — the class of bug most likely to only
appear on the second round trip — actually gets exercised. Neither test requires a
live API key or network access; both run in CI.

Tests that need `npx` (`internal/mcp/mcp_test.go`, `internal/api/api_test.go`)
self-skip when it isn't on `PATH`, so the suite degrades gracefully rather than
failing hard in an environment without Node installed.

## §12 — Eval harness

`internal/eval` + `agentforge eval` apply the same fixture-replay idea from §11
one level up: instead of asserting on `runtime.Engine` behavior from Go, an eval
*suite* (`examples/evals/*.yaml`) names a real agent config and a list of scripted
conversations, and asserts on the same data every run already persists —
`runs.state`, `runs.turn_count`, `runs.repair_count`, `tool_calls.tool_name`, the
final assistant message's text — rather than inventing new runtime machinery to
observe. `internal/provider/replay` (`Provider`/`Recorder`) is `fakeProvider`
promoted out of `_test.go`, so eval and the runtime's own tests share one
scripted-provider implementation.

Two modes: `--replay` (default, and the only mode CI runs) loads a fixture from
`testdata/fixtures/<suite>/<case>.json` for every case's model responses — no live
model, no API key. `--live` drives a real model instead, through the agent
config's own provider unless `--model` overrides it (repeatable, for a
provider/model comparison matrix); `--live --record` saves what it produced as the
fixture a later `--replay` run of the same suite will load. A case's tool calls are
always real — MCP or `tool_definitions:`-backed — even in `--replay` mode: only the
model leg is scripted, so `examples/evals/weather.yaml` still exercises the actual
Open-Meteo HTTP calls its agent makes. That's a real, deliberate CI dependency on
outbound network to a free, keyless API, not an accident — see the "Eval (replay)"
step in `.github/workflows/ci.yml`.
