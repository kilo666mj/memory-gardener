// Package gardener runs one pass: apply answered reviews, check every memory,
// judge the doubtful ones, and ask a person about each proposed change.
package gardener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/check"
	"github.com/kilo666mj/memory-gardener/internal/config"
	"github.com/kilo666mj/memory-gardener/internal/fleetglass"
	"github.com/kilo666mj/memory-gardener/internal/judge"
	"github.com/kilo666mj/memory-gardener/internal/refs"
	"github.com/kilo666mj/memory-gardener/internal/state"
	"github.com/kilo666mj/memory-gardener/internal/taskboard"
	"github.com/kilo666mj/memory-gardener/internal/wayminder"
)

// Memories is the Wayminder surface the gardener uses.
type Memories interface {
	ListAll(ctx context.Context) ([]wayminder.Memory, error)
	Supersede(ctx context.Context, id, content string) (wayminder.Memory, error)
	Forget(ctx context.Context, id string) error
}

// Tasks is the Taskboard surface the gardener uses.
type Tasks interface {
	Start(ctx context.Context, req taskboard.StartRequest) (taskboard.Task, taskboard.Run, error)
	Note(ctx context.Context, taskID, runID, body, key string) error
	Escalate(ctx context.Context, taskID string, req taskboard.EscalationRequest) (taskboard.Escalation, error)
	Get(ctx context.Context, taskID string) (taskboard.Task, error)
	Escalations(ctx context.Context, taskID string) ([]taskboard.Escalation, error)
	Messages(ctx context.Context, taskID string) ([]taskboard.Message, error)
	Claim(ctx context.Context, taskID string, version int64) (taskboard.Task, taskboard.Run, error)
	Complete(ctx context.Context, task taskboard.Task, runID, note string) error
}

// Judge decides whether a memory still holds.
type Judge interface {
	Judge(ctx context.Context, req judge.Request) (judge.Verdict, error)
}

// Checks publishes Fleetglass checks.
type Checks interface {
	Post(ctx context.Context, checks []fleetglass.Check) error
}

// Answer options offered on review questions.
const (
	OptionApply  = "Apply replacement"
	OptionForget = "Forget memory"
	OptionKeep   = "Keep unchanged"
)

// Gardener holds one run's collaborators.
type Gardener struct {
	Memories Memories
	Tasks    Tasks
	Judge    Judge
	Checks   Checks
	Checker  *check.Checker
	Policy   config.Policy
	State    *state.State
	Host     string
	Log      *slog.Logger
	Now      func() time.Time
	// DryRun reads, checks and judges, but writes nothing anywhere.
	DryRun bool
}

// Finding is one doubtful memory and what became of it this run.
type Finding struct {
	MemoryID string         `json:"memory_id"`
	Scope    string         `json:"scope"`
	Summary  string         `json:"summary"`
	Signals  []check.Signal `json:"signals"`
	Verdict  *judge.Verdict `json:"verdict,omitempty"`
	TaskID   string         `json:"task_id,omitempty"`
	Error    string         `json:"error,omitempty"`
}

// Report summarises a run.
type Report struct {
	Memories      int            `json:"memories"`
	ByScope       map[string]int `json:"by_scope"`
	Checkable     int            `json:"checkable"`
	Flagged       int            `json:"flagged"`
	Judged        int            `json:"judged"`
	StillTrue     int            `json:"still_true"`
	Unsure        int            `json:"unsure"`
	Proposed      int            `json:"proposed"`
	Deferred      int            `json:"deferred"`
	OpenReviews   int            `json:"open_reviews"`
	Applied       int            `json:"applied"`
	JudgeDown     bool           `json:"judge_down"`
	Findings      []Finding      `json:"findings"`
	ApplyFailures []string       `json:"apply_failures,omitempty"`
}

// Run performs one gardening pass.
func (g *Gardener) Run(ctx context.Context) (Report, error) {
	rep := Report{ByScope: map[string]int{}}
	if !g.DryRun {
		g.reconcile(ctx, &rep)
	}

	memories, err := g.Memories.ListAll(ctx)
	if err != nil {
		return rep, err
	}
	rep.Memories = len(memories)

	type candidate struct {
		mem    wayminder.Memory
		refs   refs.Refs
		anchor time.Time
		result check.Result
	}
	var candidates []candidate
	for _, m := range memories {
		rep.ByScope[m.Scope]++
		if _, open := g.State.Proposals[m.ID]; open {
			continue
		}
		r := refs.Extract(m.Content, m.Scope, m.CreatedAt, g.Policy)
		if len(r.Repos) == 0 && len(r.Hosts) == 0 && len(r.Transient) == 0 {
			continue
		}
		rep.Checkable++
		anchor := r.Anchor
		review, reviewed := g.State.Reviews[m.ID]
		if reviewed && review.At.After(anchor) {
			anchor = review.At
		}
		result := g.Checker.Check(ctx, r, anchor)
		if result.Max() < check.Medium {
			continue
		}
		if reviewed && review.Fingerprint == result.Fingerprint() {
			continue
		}
		rep.Flagged++
		candidates = append(candidates, candidate{mem: m, refs: r, anchor: anchor, result: result})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if a, b := candidates[i].result.Max(), candidates[j].result.Max(); a != b {
			return a > b
		}
		return candidates[i].anchor.Before(candidates[j].anchor)
	})

	for _, c := range candidates {
		f := Finding{MemoryID: c.mem.ID, Scope: c.mem.Scope, Summary: label(c.mem), Signals: c.result.Signals}
		room := len(g.State.Proposals) < g.Policy.MaxOpenProposals
		if rep.JudgeDown || rep.Judged >= g.Policy.MaxJudgementsPerRun || !room {
			rep.Deferred++
			rep.Findings = append(rep.Findings, f)
			continue
		}
		v, err := g.Judge.Judge(ctx, judge.Request{
			MemoryID: c.mem.ID, Scope: c.mem.Scope, Kind: c.mem.Kind, Content: c.mem.Content,
			Anchor: c.anchor, Signals: describe(c.result.Signals), Evidence: c.result.Evidence, Today: g.now(),
		})
		if errors.Is(err, judge.ErrUnavailable) {
			g.Log.Warn("judge unavailable; deferring the rest", "error", err)
			rep.JudgeDown = true
			rep.Deferred++
			rep.Findings = append(rep.Findings, f)
			continue
		}
		rep.Judged++
		if err != nil {
			f.Error = err.Error()
			rep.Findings = append(rep.Findings, f)
			continue
		}
		f.Verdict = &v
		fp := c.result.Fingerprint()
		if (v.Verdict == judge.NeedsUpdate || v.Verdict == judge.Obsolete) && !confident(v.Confidence, g.Policy.MinConfidence) {
			v.Reason = strings.TrimSpace(fmt.Sprintf("%s (%s, %s confidence; below the %s needed to ask.)", v.Reason, strings.ReplaceAll(v.Verdict, "_", " "), v.Confidence, g.Policy.MinConfidence))
			v.Verdict, v.Edits, v.Replacement = judge.Unsure, nil, ""
		}
		switch v.Verdict {
		case judge.StillTrue, judge.Unsure:
			if v.Verdict == judge.StillTrue {
				rep.StillTrue++
			} else {
				rep.Unsure++
			}
			if !g.DryRun {
				g.State.Reviews[c.mem.ID] = state.Review{At: g.now(), Outcome: "judged_" + v.Verdict, Fingerprint: fp}
			}
		case judge.NeedsUpdate, judge.Obsolete:
			rep.Proposed++
			if !g.DryRun {
				taskID, err := g.propose(ctx, c.mem, c.result, v, fp)
				if err != nil {
					f.Error = err.Error()
				}
				f.TaskID = taskID
			}
		}
		rep.Findings = append(rep.Findings, f)
	}
	rep.OpenReviews = len(g.State.Proposals)
	return rep, nil
}

// Publish posts the run's Fleetglass checks. runErr is the Run error, if any.
func (g *Gardener) Publish(ctx context.Context, rep Report, runErr error) error {
	if g.DryRun || g.Checks == nil {
		return nil
	}
	run := fleetglass.Check{Source: "memory-gardener", Host: g.Host, Kind: "memory_gardener", Name: "run", Status: "ok",
		Summary: fmt.Sprintf("checked %d memories, judged %d", rep.Memories, rep.Judged)}
	if runErr != nil {
		run.Status, run.Summary = "fail", runErr.Error()
	} else if rep.JudgeDown {
		run.Status, run.Summary = "warn", fmt.Sprintf("judge unavailable; %d flagged memories deferred", rep.Deferred)
	}
	staleness := fleetglass.Check{Source: "memory-gardener", Host: g.Host, Kind: "memory_gardener", Name: "staleness", Status: "ok",
		Summary: fmt.Sprintf("%d awaiting review, %d flagged, %d judged current, %d unsure, %d deferred", rep.OpenReviews, rep.Flagged, rep.StillTrue, rep.Unsure, rep.Deferred),
		Data: map[string]any{
			"memories": rep.Memories, "checkable": rep.Checkable, "flagged": rep.Flagged, "judged": rep.Judged,
			"still_true": rep.StillTrue, "unsure": rep.Unsure, "proposed": rep.Proposed, "deferred": rep.Deferred,
			"open_reviews": rep.OpenReviews, "applied": rep.Applied, "by_scope": rep.ByScope,
		}}
	if rep.OpenReviews > 0 || rep.Deferred > 0 {
		staleness.Status = "warn"
	}
	checks := []fleetglass.Check{run}
	if runErr == nil {
		checks = append(checks, staleness)
	}
	return g.Checks.Post(ctx, checks)
}

func (g *Gardener) propose(ctx context.Context, m wayminder.Memory, res check.Result, v judge.Verdict, fp string) (string, error) {
	key := "memory-gardener-" + m.ID + "-" + fp
	summary := fmt.Sprintf("Memory-gardener judged memory %s (%s) %s: %s", m.ID, m.Scope, strings.ReplaceAll(v.Verdict, "_", " "), v.Reason)
	task, run, err := g.Tasks.Start(ctx, taskboard.StartRequest{
		Title:     clip("Review memory: "+label(m), 200),
		Type:      "work",
		Summary:   clip(summary, 2000),
		Section:   g.Policy.Task.Section,
		Project:   g.Policy.Task.Project,
		Priority:  g.Policy.Task.Priority,
		Checklist: []string{"Decide on the proposed memory change", "Apply the decision in Wayminder"},

		IdempotencyKey: key + "-start",
	})
	if err != nil {
		return "", fmt.Errorf("start review task: %w", err)
	}

	notes := chunk(fmt.Sprintf("Current memory %s (scope %s, kind %s, created %s):\n\n%s", m.ID, m.Scope, m.Kind, m.CreatedAt.Format(time.DateOnly), m.Content), 3900)
	notes = append(notes, chunk("Mechanical findings:\n- "+strings.Join(describe(res.Signals), "\n- "), 3900)...)
	if v.Replacement != "" {
		notes = append(notes, chunk("Proposed replacement:\n\n"+v.Replacement, 3900)...)
	}
	for i, body := range notes {
		if err := g.Tasks.Note(ctx, task.ID, run.ID, body, fmt.Sprintf("%s-note-%d", key, i)); err != nil {
			return task.ID, fmt.Errorf("add note: %w", err)
		}
	}

	options := []string{OptionApply, OptionForget, OptionKeep}
	recommendation := OptionApply
	question := fmt.Sprintf("Memory %s looks out of date: %s\n\nThe current text and a proposed replacement are in this task's notes. Choose %q, %q or %q, or answer with the complete text you want stored instead.",
		m.ID, v.Reason, OptionApply, OptionForget, OptionKeep)
	if v.Verdict == judge.Obsolete {
		options = []string{OptionForget, OptionKeep}
		recommendation = OptionForget
		question = fmt.Sprintf("Memory %s looks obsolete: %s\n\nThe current text is in this task's notes. Choose %q or %q, or answer with the complete text you want stored instead.",
			m.ID, v.Reason, OptionForget, OptionKeep)
	}
	current, err := g.Tasks.Get(ctx, task.ID)
	if err != nil {
		return task.ID, err
	}
	esc, err := g.Tasks.Escalate(ctx, task.ID, taskboard.EscalationRequest{
		RunID: run.ID, ExpectedVersion: current.Version, Question: clip(question, 4000),
		Options: options, Recommendation: recommendation, Blocking: true,
		Answerers: g.Policy.Task.Answerers, IdempotencyKey: key + "-ask",
	})
	if err != nil {
		return task.ID, fmt.Errorf("ask review question: %w", err)
	}
	g.State.Proposals[m.ID] = state.Proposal{
		MemoryID: m.ID, TaskID: task.ID, EscalationID: esc.ID, Verdict: v.Verdict,
		Replacement: v.Replacement, Fingerprint: fp, CreatedAt: g.now(),
	}
	g.Log.Info("asked for review", "memory", m.ID, "task", task.ID, "verdict", v.Verdict)
	return task.ID, nil
}

// reconcile applies answered reviews and forgets closed ones.
func (g *Gardener) reconcile(ctx context.Context, rep *Report) {
	ids := make([]string, 0, len(g.State.Proposals))
	for id := range g.State.Proposals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := g.reconcileOne(ctx, g.State.Proposals[id], rep); err != nil {
			g.Log.Warn("could not apply review", "memory", id, "error", err)
			rep.ApplyFailures = append(rep.ApplyFailures, id+": "+err.Error())
		}
	}
}

func (g *Gardener) reconcileOne(ctx context.Context, p state.Proposal, rep *Report) error {
	task, err := g.Tasks.Get(ctx, p.TaskID)
	if errors.Is(err, taskboard.ErrNotFound) {
		g.close(p, "task_missing")
		return nil
	}
	if err != nil {
		return err
	}
	switch task.Status {
	case "done", "cancelled":
		g.close(p, "task_"+task.Status)
		return nil
	case "queued", "stale":
	default:
		return nil // Still waiting for a person, or another run holds it.
	}

	answer, answered, err := g.answer(ctx, p)
	if err != nil || !answered {
		return err
	}
	task, run, err := g.Tasks.Claim(ctx, p.TaskID, task.Version)
	if err != nil {
		return fmt.Errorf("claim review task: %w", err)
	}

	var note, outcome string
	switch {
	case strings.EqualFold(answer, OptionKeep):
		note, outcome = "Kept the memory unchanged.", "kept"
	case strings.EqualFold(answer, OptionForget):
		if err := g.Memories.Forget(ctx, p.MemoryID); err != nil && !errors.Is(err, wayminder.ErrNotLive) {
			return err
		} else if err != nil {
			note, outcome = "The memory had already changed elsewhere; nothing applied.", "changed_elsewhere"
		} else {
			note, outcome = "Forgot memory "+p.MemoryID+".", "forgotten"
		}
	default:
		content := p.Replacement
		if !strings.EqualFold(answer, OptionApply) {
			content = answer // A person's own wording.
		}
		if strings.TrimSpace(content) == "" {
			note, outcome = "No replacement text was available; nothing applied.", "kept"
			break
		}
		next, err := g.Memories.Supersede(ctx, p.MemoryID, content)
		switch {
		case errors.Is(err, wayminder.ErrNotLive):
			note, outcome = "The memory had already changed elsewhere; nothing applied.", "changed_elsewhere"
		case err != nil:
			return err
		default:
			note, outcome = fmt.Sprintf("Superseded memory %s with %s.", p.MemoryID, next.ID), "superseded"
			g.State.Reviews[next.ID] = state.Review{At: g.now(), Outcome: "written_by_review"}
		}
	}
	if err := g.Tasks.Complete(ctx, task, run.ID, note); err != nil {
		return fmt.Errorf("complete review task: %w", err)
	}
	if outcome == "superseded" || outcome == "forgotten" {
		rep.Applied++
	}
	g.close(p, outcome)
	g.Log.Info("applied review", "memory", p.MemoryID, "task", p.TaskID, "outcome", outcome)
	return nil
}

func (g *Gardener) answer(ctx context.Context, p state.Proposal) (string, bool, error) {
	escs, err := g.Tasks.Escalations(ctx, p.TaskID)
	if err != nil {
		return "", false, err
	}
	for _, e := range escs {
		if e.ID != p.EscalationID {
			continue
		}
		if e.Status == "expired" {
			return OptionKeep, true, nil
		}
		if e.Status != "answered" {
			return "", false, nil
		}
		answer := e.SelectedOption
		if e.AnswerMessageID != "" {
			msgs, err := g.Tasks.Messages(ctx, p.TaskID)
			if err != nil {
				return "", false, err
			}
			for _, m := range msgs {
				if m.ID == e.AnswerMessageID {
					answer = strings.TrimSpace(m.Body)
				}
			}
		}
		return answer, answer != "", nil
	}
	return "", false, nil
}

func (g *Gardener) close(p state.Proposal, outcome string) {
	delete(g.State.Proposals, p.MemoryID)
	g.State.Reviews[p.MemoryID] = state.Review{At: g.now(), Outcome: outcome, Fingerprint: p.Fingerprint}
}

func (g *Gardener) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// confident reports whether got meets min; an unset min means high.
func confident(got, min string) bool {
	rank := map[string]int{judge.Low: 1, judge.Medium: 2, judge.High: 3}
	if rank[min] == 0 {
		min = judge.High
	}
	return rank[got] >= rank[min]
}

func label(m wayminder.Memory) string {
	if m.Summary != "" {
		return m.Summary
	}
	first, _, _ := strings.Cut(m.Content, "\n")
	return clip(first, 100)
}

func describe(signals []check.Signal) []string {
	out := make([]string, len(signals))
	for i, s := range signals {
		out[i] = fmt.Sprintf("[%s] %s %s: %s", s.Strength, s.Kind, s.Target, s.Detail)
	}
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func chunk(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := strings.LastIndex(s[:n], "\n")
		if cut < n/2 {
			cut = n
			for cut > 0 && !utf8Start(s[cut]) {
				cut--
			}
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], "\n")
	}
	return append(out, s)
}
