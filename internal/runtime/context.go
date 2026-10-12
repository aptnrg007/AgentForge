package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/aptnrg007/AgentForge/internal/message"
	"github.com/aptnrg007/AgentForge/internal/store"
)

// ContextPolicy bounds the token budget of the message history sent to
// the model each turn. The zero value (MaxInputTokens 0) means "no
// budget" — the pre-existing behavior, unchanged unless a config opts in
// via limits.context. See docs/DESIGN.md and config.ContextConfig, which
// this is agent.Build's translation target for, same shape as
// ApprovalPolicy/ToolPolicy.
type ContextPolicy struct {
	MaxInputTokens int
	OnOverflow     string // "truncate" (default, empty) | "fail"
}

// Elision summarizes what fitHistory changed, for logging and
// EventCompaction — never persisted itself. The full, untruncated
// history this was computed from stays in SQLite regardless (ground
// rule 1): only the outbound provider request shrinks.
type Elision struct {
	EstimatedTokensBefore int
	EstimatedTokensAfter  int
	ElidedBlocks          int // tool_result bodies shrunk in place
	DroppedUnits          int // whole assistant+tool_result (or standalone) units removed
}

// charsPerToken is a deliberately conservative English-prose average —
// real BPE tokenizers run closer to 4-4.5 chars/token for prose, tighter
// still for code or JSON. Undercounting chars-per-token overcounts
// tokens, which is the safe direction for a budget whose only job is to
// stay under a limit, not to bill precisely.
const charsPerToken = 4

// estimateTokens is a conservative, provider-agnostic estimate of how
// many tokens msgs will cost, used only to decide whether fitHistory
// needs to shrink anything — not to bill or report usage (that's
// provider.Usage, persisted separately from a real API response). There
// is deliberately no real tokenizer here: CGO_ENABLED=0 rules out the
// cgo-backed ones, and a single exact count isn't available anyway
// across four providers with four different vocabularies — see
// docs/DESIGN.md's ground rules and the "what's here (and what isn't)"
// README section for the same reasoning applied elsewhere in this
// codebase.
//
// Image bytes are deliberately excluded: anthropicToolResultContent
// (internal/provider/anthropic.go) already collapses every tool
// result's images down to a text summary except on the single newest
// message in history, so at most one turn ever carries real image
// bytes — and that one turn's actual vision-token cost doesn't scale
// linearly with its base64 length anyway, so counting it would
// misestimate in both directions without meaningfully improving the
// budget's actual job, which is catching accumulated tool-result text.
func estimateTokens(msgs []message.Message) int {
	chars := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			chars += len(b.Text) + len(b.Content) + len(b.Input)
		}
	}
	return (chars + charsPerToken - 1) / charsPerToken
}

// minProtectedUnits is how many of the most recent units (see unit
// below), on top of the very first message's unit, are never touched —
// keeps the active turn and the one just before it intact even under an
// aggressive budget, so the model never loses the thread of what it was
// just doing.
const minProtectedUnits = 2

// unit is a contiguous run of one or two messages that must be kept or
// dropped together. An assistant message that made tool calls is always
// immediately followed, in settled history, by the RoleTool message
// carrying their results — stepTools appends it right after tool
// execution, and stepModel (which is the only caller of fitHistory) only
// ever runs once that pairing has settled. Splitting the two would
// orphan a tool_use with no matching tool_result, which every provider's
// translator either rejects outright (toGeminiContents' functionResponse
// name lookup — "a hard 400 if empty") or at minimum silently
// mistranslates. A message with no tool_use is its own one-message unit.
type unit struct {
	start, end int // msgs[start:end], end exclusive
}

func splitUnits(msgs []message.Message) []unit {
	var units []unit
	for i := 0; i < len(msgs); {
		end := i + 1
		if i+1 < len(msgs) && msgs[i].Role == message.RoleAssistant && hasToolUse(msgs[i]) {
			end = i + 2
		}
		units = append(units, unit{start: i, end: end})
		i = end
	}
	return units
}

func hasToolUse(m message.Message) bool {
	for _, b := range m.Content {
		if b.Type == message.BlockToolUse {
			return true
		}
	}
	return false
}

// protectedUnits marks unit 0 (holds the first message — the run's
// original request) and the minProtectedUnits most recent units as
// never eligible for shrinking or dropping.
func protectedUnits(n int) []bool {
	p := make([]bool, n)
	if n > 0 {
		p[0] = true
	}
	for i := n - minProtectedUnits; i < n; i++ {
		if i >= 0 {
			p[i] = true
		}
	}
	return p
}

const elisionMarkerSuffix = " bytes elided to fit the context budget]"

// elisionMarker replaces an elided tool_result's body. Matches the house
// style for "content removed on purpose" elsewhere in this codebase —
// internal/mcp/content.go's "[image omitted: ...]" and
// summarizeToolResultParts' "[+N image(s)]".
func elisionMarker(n int) string {
	return fmt.Sprintf("[%d%s", n, elisionMarkerSuffix)
}

func isElisionMarker(s string) bool {
	return strings.HasPrefix(s, "[") && strings.HasSuffix(s, elisionMarkerSuffix)
}

// shrinkToolResults returns a copy of m (which must be a RoleTool
// message) with every not-yet-shrunk tool_result body replaced by a
// byte-count marker, preserving the block's ToolUseID and IsError — the
// pairing a tool_use needs on the wire — and dropping ToolResultParts,
// the rich multi-part form only Anthropic's translator reads, and only
// for the single newest message in history (anthropic.go's keepImages);
// a message eligible for shrinking is by construction never the newest.
// n is how many blocks were actually changed; 0 means m had nothing left
// to shrink.
func shrinkToolResults(m message.Message) (message.Message, int) {
	content := append([]message.ContentBlock(nil), m.Content...)
	n := 0
	for i, b := range content {
		if b.Type != message.BlockToolResult || len(b.Content) == 0 || isElisionMarker(b.Content) {
			continue
		}
		content[i] = message.ContentBlock{
			Type:      message.BlockToolResult,
			ToolUseID: b.ToolUseID,
			IsError:   b.IsError,
			Content:   elisionMarker(len(b.Content)),
		}
		n++
	}
	if n == 0 {
		return m, 0
	}
	return message.Message{Role: m.Role, Content: content}, n
}

// fitHistory shrinks/drops the oldest eligible parts of msgs until its
// estimated token cost is under pol.MaxInputTokens, applied in two
// passes — shrink tool_result bodies in place first (cheaper to the
// model's continuity: every tool_use/tool_result pair stays present,
// just smaller), then drop whole oldest units outright only if shrinking
// wasn't enough. changed is false (and fitted == msgs unchanged) when
// pol disables the budget (MaxInputTokens <= 0) or the estimate already
// fits — the exact no-op this needs to be when limits.context is absent
// from a config entirely, since agent.Build then leaves pol at its zero
// value.
//
// fitHistory never modifies msgs itself and never touches SQLite — the
// full, untruncated history stays persisted; see stepModel for how its
// result is used only for the outbound provider request.
func fitHistory(msgs []message.Message, pol ContextPolicy) (fitted []message.Message, el Elision, changed bool) {
	before := estimateTokens(msgs)
	el = Elision{EstimatedTokensBefore: before, EstimatedTokensAfter: before}
	if pol.MaxInputTokens <= 0 || before <= pol.MaxInputTokens {
		return msgs, el, false
	}

	units := splitUnits(msgs)
	protected := protectedUnits(len(units))
	fitted = append([]message.Message(nil), msgs...)

	fits := func() bool { return estimateTokens(fitted) <= pol.MaxInputTokens }

	// Pass 1 — shrink the oldest eligible tool_result bodies in place.
	for ui := 0; ui < len(units) && !fits(); ui++ {
		if protected[ui] {
			continue
		}
		for mi := units[ui].start; mi < units[ui].end; mi++ {
			if fitted[mi].Role != message.RoleTool {
				continue
			}
			shrunk, n := shrinkToolResults(fitted[mi])
			if n == 0 {
				continue
			}
			fitted[mi] = shrunk
			el.ElidedBlocks += n
			if fits() {
				break
			}
		}
	}

	// Pass 2 — if shrinking wasn't enough, drop whole oldest eligible
	// units outright. Never a lone message: units[*].start/end always
	// span a tool_use/tool_result pair together, so a drop here can
	// never orphan one side of that pairing.
	if !fits() {
		dropped := make([]bool, len(units))
		apply := func() []message.Message {
			var out []message.Message
			for ui, u := range units {
				if dropped[ui] {
					continue
				}
				out = append(out, fitted[u.start:u.end]...)
			}
			return out
		}
		for ui := 0; ui < len(units); ui++ {
			if estimateTokens(apply()) <= pol.MaxInputTokens {
				break
			}
			if protected[ui] {
				continue
			}
			dropped[ui] = true
			el.DroppedUnits++
		}
		fitted = apply()
	}

	el.EstimatedTokensAfter = estimateTokens(fitted)
	return fitted, el, true
}

// enforceContextBudget applies e.cfg.Context to msgs ahead of a provider
// call. done reports whether stepModel should return (state, err)
// immediately without ever calling the provider — true only for
// OnOverflow: "fail" over budget, where deliberately there is no attempt
// to fit: unlike "truncate", "fail" means the operator wants to know the
// run can't continue under its configured budget, not have agentforge
// decide what to drop on its behalf. When done is false, fitted is what
// stepModel should actually send — msgs unchanged if the budget is off,
// already satisfied, or fitHistory found nothing to change.
func (e *Engine) enforceContextBudget(ctx context.Context, run *store.Run, msgs []message.Message) (fitted []message.Message, state State, err error, done bool) {
	pol := e.cfg.Context
	if pol.MaxInputTokens <= 0 {
		return msgs, "", nil, false
	}

	if pol.OnOverflow == "fail" {
		if est := estimateTokens(msgs); est > pol.MaxInputTokens {
			reason := fmt.Sprintf("context budget exceeded: estimated %d tokens, limit %d (limits.context.max_input_tokens)", est, pol.MaxInputTokens)
			pctx, cancel := terminalCtx(ctx)
			uerr := e.store.UpdateRun(pctx, run.ID, string(StateFailed), run.TurnCount, run.RepairCount, &reason)
			cancel()
			if uerr != nil {
				return nil, "", uerr, true
			}
			return nil, StateFailed, nil, true
		}
		return msgs, "", nil, false
	}

	fitted, el, changed := fitHistory(msgs, pol)
	if !changed {
		return msgs, "", nil, false
	}

	e.logger.Info("runtime: compacted history to fit context budget",
		"run_id", run.ID,
		"tokens_before", el.EstimatedTokensBefore,
		"tokens_after", el.EstimatedTokensAfter,
		"elided_blocks", el.ElidedBlocks,
		"dropped_units", el.DroppedUnits,
	)
	e.emit(Event{
		Kind:         EventCompaction,
		TokensBefore: el.EstimatedTokensBefore,
		TokensAfter:  el.EstimatedTokensAfter,
		ElidedBlocks: el.ElidedBlocks,
		DroppedUnits: el.DroppedUnits,
	})
	return fitted, "", nil, false
}
