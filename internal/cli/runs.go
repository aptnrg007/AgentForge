package cli

import (
	"context"

	"github.com/spf13/cobra"

	"agentforge/internal/store"
)

func newRunsCmd() *cobra.Command {
	var server, dbPath string

	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's full trace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runsGet(cmd.Context(), server, dbPath, args[0])
		},
	}

	cmd := &cobra.Command{Use: "runs", Short: "Inspect runs"}
	cmd.PersistentFlags().StringVar(&server, "server", "", "daemon URL, e.g. http://localhost:8080 (defaults to the local --db store)")
	cmd.PersistentFlags().StringVar(&dbPath, "db", "agentforge.db", "path to the SQLite run store (used when --server is not set)")
	cmd.AddCommand(get)
	return cmd
}

func runsGet(ctx context.Context, server, dbPath, id string) error {
	if server != "" {
		var trace remoteRunTrace
		if err := apiGet(ctx, server+"/v1/runs/"+id, &trace); err != nil {
			return err
		}
		printRunTrace(trace.RunID, trace.State, trace.TurnCount, trace.Error, trace.Messages)
		return nil
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	run, err := st.GetRun(ctx, id)
	if err != nil {
		return err
	}
	msgs, err := st.ListMessages(ctx, id)
	if err != nil {
		return err
	}
	printRunTrace(run.ID, run.State, run.TurnCount, run.Error, msgs)
	return nil
}
