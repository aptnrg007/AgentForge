package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentforge/internal/config"
	"agentforge/internal/runtime"
	"agentforge/internal/schema"
)

// minimalEnvVars are copied from the daemon's own environment into every
// command-backed tool's child process. Deliberately not os.Environ(): a
// model-driven exec that inherited the full parent environment would
// hand it every API key and secret the daemon holds. Anything a
// definition needs beyond these is passed explicitly via command.env.
var minimalEnvVars = []string{"PATH", "HOME", "LANG"}

// commandExecutor builds the runtime.ToolExecutor for a command:-backed
// ToolDefinition. name is only used in error messages; sourceDir is the
// owning config's directory (config.Config.SourceDir), against which a
// relative workdir resolves.
func commandExecutor(name string, cfg config.CommandToolConfig, sourceDir string, validator *schema.Validator) runtime.ToolExecutor {
	maxBytes := cfg.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	return func(ctx context.Context, input json.RawMessage) (string, error) {
		if err := validateInput(input, validator); err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		data, err := decodeInput(input)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}

		argv := make([]string, len(cfg.Argv))
		argv[0] = cfg.Argv[0] // literal — config.CommandToolConfig.validate forbids a template here
		for i := 1; i < len(cfg.Argv); i++ {
			rendered, err := render(cfg.Argv[i], data)
			if err != nil {
				return "", fmt.Errorf("%s: argv[%d]: %w", name, i, err)
			}
			argv[i] = rendered
		}

		workdir, err := render(cfg.Workdir, data)
		if err != nil {
			return "", fmt.Errorf("%s: workdir: %w", name, err)
		}
		if workdir != "" && !filepath.IsAbs(workdir) && sourceDir != "" {
			workdir = filepath.Join(sourceDir, workdir)
		}

		env, err := renderMap(cfg.Env, data)
		if err != nil {
			return "", fmt.Errorf("%s: env: %w", name, err)
		}

		stdin, err := render(cfg.Stdin, data)
		if err != nil {
			return "", fmt.Errorf("%s: stdin: %w", name, err)
		}

		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = workdir
		cmd.Env = childEnv(env)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}

		var stdout, stderr cappedBuffer
		stdout.max, stderr.max = maxBytes, maxBytes
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		// Kill the whole process group on timeout/cancellation, not
		// just the direct child — otherwise a command that spawns
		// its own children (a shell script, a wrapper) orphans them
		// when tool_policy's deadline fires. Unix-only; see
		// command_unix.go / command_other.go.
		configureProcessGroup(cmd)

		runErr := cmd.Run()
		out := stdout.String()
		if stdout.truncated {
			out += "\n[truncated]"
		}

		if runErr != nil {
			errText := stderr.String()
			if stderr.truncated {
				errText += "\n[truncated]"
			}
			return "", fmt.Errorf("%s: %w: %s", name, runErr, strings.TrimSpace(errText))
		}
		return out, nil
	}
}

// childEnv builds the process environment for a command-backed tool:
// PATH/HOME/LANG copied from the daemon's own environment, overlaid with
// the definition's rendered env: map. See minimalEnvVars.
func childEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(minimalEnvVars)+len(overrides))
	for _, k := range minimalEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}

// cappedBuffer is an io.Writer that keeps at most max bytes and silently
// discards (but tracks) anything beyond that — used for a command's
// stdout/stderr so a runaway process can't blow up memory or a tool
// result's size.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := c.max - c.buf.Len()
	if room <= 0 {
		if n > 0 {
			c.truncated = true
		}
		return n, nil
	}
	if room < len(p) {
		c.buf.Write(p[:room])
		c.truncated = true
	} else {
		c.buf.Write(p)
	}
	return n, nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
