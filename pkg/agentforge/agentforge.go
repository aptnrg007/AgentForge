// Package agentforge is the public, importable surface over AgentForge's
// runtime: load an agent config, run it, resolve approvals, cancel it,
// and subscribe to fine-grained progress events, all in-process — no
// `agentforge serve` daemon required. Every other entry point
// (internal/cli, internal/api) is a thin wrapper over the same
// internal/agent.Build + internal/runtime.Engine + internal/store this
// package uses; this is that wiring, exported.
//
//	ag, err := agentforge.Load("agent.yaml")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer ag.Close()
//
//	run, err := ag.Run(context.Background(), "Find the latest issues")
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Println(run.Output)
//
// A Run that stops at StateAwaitingApproval (see Run.Pending) is resolved
// with Approve/Deny, then continued with Resume — the same
// approval-then-resume shape `agentforge runs approve`/`resume` use.
package agentforge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

// Agent is a loaded agent config, ready to run. It owns a local SQLite
// store and an MCP server registry for the lifetime of the process (or
// until Close), so repeated Run/Resume calls reuse already-connected MCP
// servers instead of reconnecting each time — the same registry
// `agentforge serve` uses across every HTTP request.
type Agent struct {
	cfg      *config.Config
	st       *store.Store
	registry *mcp.Registry
	pf       agent.ProviderFactory
}

type loadOptions struct {
	dbPath string
}

// LoadOption configures Load.
type LoadOption func(*loadOptions)

// WithDB overrides the local SQLite store path Load opens. The default
// is the same one every CLI command uses (~/.agentforge/agentforge.db),
// so runs started through the SDK show up in `agentforge runs list` and
// vice versa.
func WithDB(path string) LoadOption {
	return func(o *loadOptions) { o.dbPath = path }
}

// Load reads and validates the agent config at path, opens its local
// run store (creating it on first use), and registers the config so
// `agentforge runs`/`agents` can see runs started through the returned
// Agent. Call Close when done with it.
func Load(path string, opts ...LoadOption) (*Agent, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	rawYAML, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agentforge: read %s: %w", path, err)
	}

	o := loadOptions{dbPath: defaultDBPath()}
	for _, opt := range opts {
		opt(&o)
	}
	if err := ensureDBDir(o.dbPath); err != nil {
		return nil, err
	}
	st, err := store.Open(o.dbPath)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	if err := st.UpsertAgent(ctx, cfg.Name, string(rawYAML)); err != nil {
		st.Close()
		return nil, err
	}

	return &Agent{
		cfg:      cfg,
		st:       st,
		registry: mcp.NewRegistry(slog.Default()),
		pf:       agent.DefaultProviderFactory,
	}, nil
}

// Close closes the Agent's MCP server registry (terminating any MCP
// subprocess it started) and its store.
func (a *Agent) Close() error {
	a.registry.Close()
	return a.st.Close()
}

// defaultDBPath mirrors internal/cli/dbpath.go's defaultDBPath — kept as
// a separate copy (not imported: dbPath is unexported, and internal/cli
// is a command-line binary's internals, not a library this package
// should depend on) rather than exported from internal/cli for one
// caller. Same default, so a run started via the SDK and one started via
// the CLI land in the same store unless WithDB overrides it.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "agentforge.db"
	}
	return filepath.Join(home, ".agentforge", "agentforge.db")
}

func ensureDBDir(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}
