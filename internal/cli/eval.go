package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"agentforge/internal/eval"
)

func newEvalCmd() *cobra.Command {
	var (
		live   bool
		record bool
		models []string
		root   string
	)

	cmd := &cobra.Command{
		Use:   "eval <suite.yaml | dir>",
		Short: "Run an eval suite against a scripted fixture or a live model",
		Long: `Run one eval suite file, or every *.yaml suite in a directory, and
report each case's pass/fail against its expect: block.

Replay mode (the default) drives every case against a recorded fixture
under testdata/fixtures/ — no live model, no API key, deterministic; this
is what CI runs. --live hits a real model instead, through the agent
config's own provider unless --model overrides it (repeatable, for a
comparison matrix); add --record to save what --live produced as the
fixture --replay mode will use next time.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if record && !live {
				return fmt.Errorf("--record only applies with --live")
			}
			if len(models) > 0 && !live {
				return fmt.Errorf("--model only applies with --live")
			}
			mode := eval.ModeReplay
			if live {
				mode = eval.ModeLive
			}
			opts := eval.RunOptions{Mode: mode, Root: root, Models: models, Record: record}
			return runEval(cmd.Context(), args[0], opts)
		},
	}

	cmd.Flags().BoolVar(&live, "live", false, "hit a real model instead of a recorded fixture")
	cmd.Flags().BoolVar(&record, "record", false, "with --live, save each case's model responses as its replay fixture")
	cmd.Flags().StringArrayVar(&models, "model", nil, `with --live, override the model to run against (repeatable); "name" keeps the config's provider, "provider:name" overrides both`)
	cmd.Flags().StringVar(&root, "root", ".", "root directory eval fixtures are resolved relative to (testdata/fixtures/...)")
	return cmd
}

func runEval(ctx context.Context, path string, opts eval.RunOptions) error {
	suitePaths, err := suiteFiles(path)
	if err != nil {
		return err
	}

	anyFailed := false
	for _, sp := range suitePaths {
		suite, err := eval.Load(sp)
		if err != nil {
			return err
		}
		result, err := eval.RunSuite(ctx, suite, opts)
		if err != nil {
			return err
		}
		printSuiteResult(result)
		if !result.Passed() {
			anyFailed = true
		}
	}

	if anyFailed {
		return fmt.Errorf("eval: one or more cases failed")
	}
	return nil
}

// suiteFiles resolves path to the list of suite files to run: itself, if
// it's a file, or every top-level *.yaml/*.yml in it, if it's a directory.
func suiteFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext == ".yaml" || ext == ".yml" {
			files = append(files, filepath.Join(path, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("eval: %s: no *.yaml suite files found", path)
	}
	return files, nil
}

func printSuiteResult(r *eval.SuiteResult) {
	fmt.Printf("%s\n", r.Suite)
	for _, run := range r.Runs {
		label := run.Model
		if label == "" {
			label = "(config default)"
		}
		if len(r.Runs) > 1 {
			fmt.Printf("  model: %s\n", label)
		}
		for _, c := range run.Cases {
			printCaseResult(c, len(r.Runs) > 1)
		}
	}
}

func printCaseResult(c eval.CaseResult, indent bool) {
	prefix := "  "
	if indent {
		prefix = "    "
	}
	if c.RunErr != nil {
		fmt.Printf("%sFAIL  %s  (error: %s)\n", prefix, c.Case.Name, c.RunErr)
		return
	}
	if len(c.Failures) == 0 {
		fmt.Printf("%sPASS  %s\n", prefix, c.Case.Name)
		return
	}
	fmt.Printf("%sFAIL  %s\n", prefix, c.Case.Name)
	for _, f := range c.Failures {
		fmt.Printf("%s        - %s\n", prefix, f)
	}
}
