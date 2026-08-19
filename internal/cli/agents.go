package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"agentforge/internal/store"
)

func newAgentsCmd() *cobra.Command {
	var server, dbPath string

	list := &cobra.Command{
		Use:   "list",
		Short: "List agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return agentsList(cmd.Context(), server, dbPath)
		},
	}
	get := &cobra.Command{
		Use:   "get <name>",
		Short: "Print one agent's YAML",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agentsGet(cmd.Context(), server, dbPath, args[0])
		},
	}
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agentsDelete(cmd.Context(), server, dbPath, args[0])
		},
	}

	cmd := &cobra.Command{Use: "agents", Short: "Manage agents"}
	cmd.PersistentFlags().StringVar(&server, "server", "", "daemon URL, e.g. http://localhost:8080 (defaults to the local --db store)")
	cmd.PersistentFlags().StringVar(&dbPath, "db", defaultDBPath(), "path to the SQLite run store (used when --server is not set)")
	cmd.AddCommand(list, get, del)
	return cmd
}

func agentsList(ctx context.Context, server, dbPath string) error {
	if server != "" {
		var agents []remoteAgent
		if err := apiGet(ctx, server+"/v1/agents", &agents); err != nil {
			return err
		}
		for _, a := range agents {
			fmt.Println(a.Name)
		}
		return nil
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	agents, err := st.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		fmt.Println(a.Name)
	}
	return nil
}

func agentsGet(ctx context.Context, server, dbPath, name string) error {
	if server != "" {
		var ag remoteAgent
		if err := apiGet(ctx, server+"/v1/agents/"+name, &ag); err != nil {
			return err
		}
		fmt.Print(ag.YAML)
		return nil
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ag, err := st.GetAgent(ctx, name)
	if err != nil {
		return err
	}
	fmt.Print(ag.YAML)
	return nil
}

func agentsDelete(ctx context.Context, server, dbPath, name string) error {
	if server != "" {
		return apiDelete(ctx, server+"/v1/agents/"+name)
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	return st.DeleteAgent(ctx, name)
}
