// Command memory-gardener checks Wayminder memories against the repositories
// and hosts they describe, asks a local model whether doubtful ones still
// hold, and opens a Taskboard review for each proposed change.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/check"
	"github.com/kilo666mj/memory-gardener/internal/config"
	"github.com/kilo666mj/memory-gardener/internal/fleetglass"
	"github.com/kilo666mj/memory-gardener/internal/gardener"
	"github.com/kilo666mj/memory-gardener/internal/gitrepo"
	"github.com/kilo666mj/memory-gardener/internal/judge"
	"github.com/kilo666mj/memory-gardener/internal/state"
	"github.com/kilo666mj/memory-gardener/internal/taskboard"
	"github.com/kilo666mj/memory-gardener/internal/wayminder"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "memory-gardener:", err)
		os.Exit(1)
	}
}

func run(args []string) (err error) {
	fs := flag.NewFlagSet("memory-gardener", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "check and judge, but write nothing to Wayminder, Taskboard, Fleetglass or the state file")
	noJudge := fs.Bool("no-judge", false, "skip the model; report mechanical findings only (implies -dry-run)")
	asJSON := fs.Bool("json", false, "print the run report as JSON")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *noJudge {
		*dryRun = true
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if !*dryRun && (cfg.TaskboardURL == "" || cfg.TaskboardToken == "") {
		return errors.New("MEMORY_GARDENER_TASKBOARD_URL and MEMORY_GARDENER_TASKBOARD_TOKEN are required unless -dry-run")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mem, err := wayminder.Dial(ctx, cfg.WayminderURL, cfg.WayminderToken, version)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := mem.Close(); cerr != nil && err == nil {
			logger.Warn("close wayminder session", "error", cerr)
		}
	}()

	repos := map[string]check.Repo{}
	for _, r := range cfg.Policy.Repos {
		m, err := gitrepo.Sync(ctx, filepath.Join(cfg.DataDir, "mirrors", r.Name+".git"), r.URL)
		if err != nil {
			// One unreachable repository should not stop the others' checks.
			logger.Warn("mirror sync failed; skipping repository", "repo", r.Name, "error", err)
			continue
		}
		repos[r.Name] = m
	}

	stateFile := filepath.Join(cfg.DataDir, "state.json")
	st, err := state.Load(stateFile)
	if err != nil {
		return err
	}

	var j gardener.Judge = &judge.Client{
		BaseURL: cfg.JudgeURL, Model: cfg.JudgeModel, APIKey: cfg.JudgeAPIKey,
		HTTP: &http.Client{Timeout: cfg.JudgeTimeout},
	}
	if *noJudge {
		j = offline{}
	}
	g := &gardener.Gardener{
		Memories: mem,
		Tasks:    &taskboard.Client{BaseURL: cfg.TaskboardURL, Token: cfg.TaskboardToken, SessionKey: "memory-gardener@" + cfg.FleetglassHost},
		Judge:    j,
		Checks:   &fleetglass.Client{BaseURL: cfg.FleetglassURL, Token: cfg.FleetglassToken},
		Checker: &check.Checker{
			Repos: repos, Resolve: check.DNSResolver, TransientAgeDays: cfg.Policy.TransientAgeDays,
		},
		Policy: cfg.Policy,
		State:  st,
		Host:   cfg.FleetglassHost,
		Log:    logger,
		DryRun: *dryRun,
	}

	rep, runErr := g.Run(ctx)
	if !*dryRun {
		if err := st.Save(stateFile); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("save state: %w", err))
		}
	}
	if err := g.Publish(ctx, rep, runErr); err != nil {
		logger.Warn("publish fleetglass checks", "error", err)
	}
	if runErr != nil {
		return runErr
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(rep, *dryRun)
	return nil
}

// offline is the judge for -no-judge: every flagged memory is deferred.
type offline struct{}

func (offline) Judge(context.Context, judge.Request) (judge.Verdict, error) {
	return judge.Verdict{}, fmt.Errorf("%w: disabled by -no-judge", judge.ErrUnavailable)
}

func printReport(rep gardener.Report, dryRun bool) {
	mode := ""
	if dryRun {
		mode = " (dry run: nothing written)"
	}
	fmt.Printf("memory-gardener %s%s\n", time.Now().Format(time.DateTime), mode)
	fmt.Printf("%d memories in %d scopes; %d checkable; %d flagged\n", rep.Memories, len(rep.ByScope), rep.Checkable, rep.Flagged)
	fmt.Printf("judged %d: %d still true, %d unsure, %d proposed changes; %d deferred\n", rep.Judged, rep.StillTrue, rep.Unsure, rep.Proposed, rep.Deferred)
	fmt.Printf("%d reviews open; %d applied this run\n", rep.OpenReviews, rep.Applied)
	if rep.JudgeDown {
		fmt.Println("judge was unavailable; flagged memories were deferred")
	}
	for _, f := range rep.Findings {
		fmt.Printf("\n%s  %s  %s\n", f.MemoryID, f.Scope, f.Summary)
		for _, s := range f.Signals {
			fmt.Printf("  [%s] %s %s: %s\n", s.Strength, s.Kind, s.Target, s.Detail)
		}
		if f.Verdict != nil {
			fmt.Printf("  verdict: %s: %s\n", f.Verdict.Verdict, f.Verdict.Reason)
			if f.Verdict.Replacement != "" {
				fmt.Printf("  replacement:\n    %s\n", strings.ReplaceAll(f.Verdict.Replacement, "\n", "\n    "))
			}
		}
		if f.TaskID != "" {
			fmt.Printf("  review task: %s\n", f.TaskID)
		}
		if f.Error != "" {
			fmt.Printf("  error: %s\n", f.Error)
		}
	}
	for _, failure := range rep.ApplyFailures {
		fmt.Printf("apply failure: %s\n", failure)
	}
}
