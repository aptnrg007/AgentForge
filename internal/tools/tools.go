package tools

import (
	"fmt"

	"agentforge/internal/config"
	"agentforge/internal/runtime"
	"agentforge/internal/schema"
)

// defaultMaxBytes caps a defined tool's captured output (HTTP response
// body or command stdout) when the config leaves max_response_bytes /
// max_output_bytes at 0. A tool result this large is already well past
// useful context for a model; the cap exists so a runaway response can't
// blow up the run's token budget or memory.
const defaultMaxBytes = 64 * 1024

// Build turns cfg.ToolDefinitions into runtime.Tool values, one per
// definition. Every duration/schema/template string here was already
// validated by config.Parse (config.ToolDefinition.validate), so the
// only errors Build itself can hit are schema.Compile re-runs — kept as
// real errors rather than dropped, unlike agent.toolPolicy's duration
// re-parse, because a compile failure here is more consequential (it
// would silently accept any input) even though it's just as unreachable
// in practice.
func Build(cfg *config.Config) ([]runtime.Tool, error) {
	if len(cfg.ToolDefinitions) == 0 {
		return nil, nil
	}

	out := make([]runtime.Tool, 0, len(cfg.ToolDefinitions))
	for _, def := range cfg.ToolDefinitions {
		validator, err := schema.Compile(def.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool_definitions (%s): input_schema: %w", def.Name, err)
		}

		var exec runtime.ToolExecutor
		switch {
		case def.HTTP != nil:
			exec = httpExecutor(def.Name, *def.HTTP, validator)
		case def.Command != nil:
			exec = commandExecutor(def.Name, *def.Command, cfg.SourceDir, validator)
		default:
			// Unreachable: config.ToolDefinition.validate requires
			// exactly one of HTTP/Command.
			return nil, fmt.Errorf("tool_definitions (%s): neither http nor command set", def.Name)
		}

		out = append(out, runtime.Tool{
			Name:             def.Name,
			Description:      def.Description,
			InputSchema:      def.InputSchema,
			ReadOnlyHint:     def.ReadOnly,
			RequiresApproval: def.Command != nil,
			Execute:          exec,
		})
	}
	return out, nil
}

// validateInput checks a tool call's input against the tool's compiled
// input_schema before any template rendering happens, so a missing
// required field comes back as a clear schema-violation message ("city
// is required") rather than a confusing template or exec failure.
func validateInput(input []byte, validator *schema.Validator) error {
	problems := validator.Validate(input)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("input does not match input_schema: %v", problems)
}
