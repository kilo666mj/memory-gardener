package gardener

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/check"
	"github.com/kilo666mj/memory-gardener/internal/config"
	"github.com/kilo666mj/memory-gardener/internal/judge"
	"github.com/kilo666mj/memory-gardener/internal/state"
	"github.com/kilo666mj/memory-gardener/internal/taskboard"
	"github.com/kilo666mj/memory-gardener/internal/wayminder"
)

type fakeMemories struct {
	live       map[string]wayminder.Memory
	superseded map[string]string
	forgotten  []string
	next       int
}

func (f *fakeMemories) ListAll(context.Context) ([]wayminder.Memory, error) {
	var out []wayminder.Memory
	for _, m := range f.live {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeMemories) Supersede(_ context.Context, id, content string) (wayminder.Memory, error) {
	old, ok := f.live[id]
	if !ok {
		return wayminder.Memory{}, fmt.Errorf("%w: live memory %s not found", wayminder.ErrNotLive, id)
	}
	delete(f.live, id)
	f.next++
	m := old
	m.ID, m.Content = fmt.Sprintf("NEW%d", f.next), content
	f.live[m.ID] = m
	f.superseded[id] = content
	return m, nil
}

func (f *fakeMemories) Forget(_ context.Context, id string) error {
	if _, ok := f.live[id]; !ok {
		return fmt.Errorf("%w: %s", wayminder.ErrNotLive, id)
	}
	delete(f.live, id)
	f.forgotten = append(f.forgotten, id)
	return nil
}

type fakeTasks struct {
	tasks     map[string]*taskboard.Task
	escs      map[string]*taskboard.Escalation
	messages  map[string][]taskboard.Message
	questions map[string]taskboard.EscalationRequest
	completed map[string]string
	n         int
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{tasks: map[string]*taskboard.Task{}, escs: map[string]*taskboard.Escalation{},
		messages: map[string][]taskboard.Message{}, questions: map[string]taskboard.EscalationRequest{}, completed: map[string]string{}}
}

func (f *fakeTasks) id(prefix string) string { f.n++; return fmt.Sprintf("%s%d", prefix, f.n) }

func (f *fakeTasks) Start(_ context.Context, req taskboard.StartRequest) (taskboard.Task, taskboard.Run, error) {
	t := &taskboard.Task{ID: f.id("T"), Status: "active", Version: 1}
	for _, label := range req.Checklist {
		t.Items = append(t.Items, taskboard.Item{ID: f.id("I"), Label: label, Status: "todo"})
	}
	f.tasks[t.ID] = t
	return *t, taskboard.Run{ID: f.id("R")}, nil
}

func (f *fakeTasks) Note(_ context.Context, taskID, _, body, _ string) error {
	f.messages[taskID] = append(f.messages[taskID], taskboard.Message{ID: f.id("M"), Kind: "note", Body: body})
	f.tasks[taskID].Version++
	return nil
}

func (f *fakeTasks) Escalate(_ context.Context, taskID string, req taskboard.EscalationRequest) (taskboard.Escalation, error) {
	t := f.tasks[taskID]
	if req.ExpectedVersion != t.Version {
		return taskboard.Escalation{}, &taskboard.StatusError{Status: 409, Body: "version conflict"}
	}
	e := &taskboard.Escalation{ID: f.id("E"), Status: "open"}
	f.escs[taskID] = e
	f.questions[taskID] = req
	t.Status = "waiting"
	t.Version++
	return *e, nil
}

// answer simulates a person answering in Taskboard.
func (f *fakeTasks) answer(taskID, text string) {
	msg := taskboard.Message{ID: f.id("M"), Kind: "answer", Body: text}
	f.messages[taskID] = append(f.messages[taskID], msg)
	e := f.escs[taskID]
	e.Status, e.AnswerMessageID = "answered", msg.ID
	for _, o := range f.questions[taskID].Options {
		if o == text {
			e.SelectedOption = o
		}
	}
	f.tasks[taskID].Status = "queued"
	f.tasks[taskID].Version++
}

func (f *fakeTasks) Get(_ context.Context, id string) (taskboard.Task, error) {
	t, ok := f.tasks[id]
	if !ok {
		return taskboard.Task{}, &taskboard.StatusError{Status: 404}
	}
	return *t, nil
}

func (f *fakeTasks) Escalations(_ context.Context, id string) ([]taskboard.Escalation, error) {
	if e, ok := f.escs[id]; ok {
		return []taskboard.Escalation{*e}, nil
	}
	return nil, nil
}

func (f *fakeTasks) Messages(_ context.Context, id string) ([]taskboard.Message, error) {
	return f.messages[id], nil
}

func (f *fakeTasks) Claim(_ context.Context, id string, version int64) (taskboard.Task, taskboard.Run, error) {
	t := f.tasks[id]
	if t.Version != version || (t.Status != "queued" && t.Status != "stale") {
		return taskboard.Task{}, taskboard.Run{}, &taskboard.StatusError{Status: 409}
	}
	t.Status = "active"
	t.Version++
	return *t, taskboard.Run{ID: f.id("R")}, nil
}

func (f *fakeTasks) Complete(_ context.Context, task taskboard.Task, _, note string) error {
	t := f.tasks[task.ID]
	if t.Version != task.Version {
		return &taskboard.StatusError{Status: 409}
	}
	t.Status = "done"
	f.completed[task.ID] = note
	return nil
}

type fakeJudge struct {
	verdicts map[string]judge.Verdict
	calls    int
	down     bool
}

func (f *fakeJudge) Judge(_ context.Context, req judge.Request) (judge.Verdict, error) {
	if f.down {
		return judge.Verdict{}, fmt.Errorf("%w: connection refused", judge.ErrUnavailable)
	}
	f.calls++
	return f.verdicts[req.MemoryID], nil
}

// flaggingResolver makes every host lookup fail, so any memory naming a
// .internal host gets a strong signal without needing git.
func flaggingResolver(context.Context, string) error { return fmt.Errorf("no such host") }

type harness struct {
	mem   *fakeMemories
	tasks *fakeTasks
	judge *fakeJudge
	st    *state.State
	g     *Gardener
}

func newHarness(memories ...wayminder.Memory) *harness {
	h := &harness{
		mem:   &fakeMemories{live: map[string]wayminder.Memory{}, superseded: map[string]string{}},
		tasks: newFakeTasks(),
		judge: &fakeJudge{verdicts: map[string]judge.Verdict{}},
		st:    &state.State{Proposals: map[string]state.Proposal{}, Reviews: map[string]state.Review{}},
	}
	for _, m := range memories {
		h.mem.live[m.ID] = m
	}
	h.g = &Gardener{
		Memories: h.mem, Tasks: h.tasks, Judge: h.judge,
		Checker: &check.Checker{Resolve: flaggingResolver},
		Policy:  config.Policy{HostSuffixes: []string{".internal"}, MaxJudgementsPerRun: 10, MaxOpenProposals: 10, Task: config.TaskPolicy{Answerers: []string{"user:me"}}},
		State:   h.st,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC) },
	}
	return h
}

func mem(id, content string) wayminder.Memory {
	return wayminder.Memory{ID: id, Content: content, Scope: "personal", Kind: "fact", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
}

func (h *harness) run(t *testing.T) Report {
	t.Helper()
	rep, err := h.g.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestProposeThenApplyApprovedReplacement(t *testing.T) {
	h := newHarness(mem("A", "Callers use old.internal:11434."), mem("B", "Nothing checkable here."))
	h.judge.verdicts["A"] = judge.Verdict{Confidence: judge.High, Verdict: judge.NeedsUpdate, Reason: "old.internal no longer resolves", Replacement: "Callers use new.internal:11434."}

	rep := h.run(t)
	if rep.Memories != 2 || rep.Checkable != 1 || rep.Flagged != 1 || rep.Proposed != 1 || rep.OpenReviews != 1 {
		t.Fatalf("report = %+v", rep)
	}
	p, ok := h.st.Proposals["A"]
	if !ok {
		t.Fatal("no proposal recorded")
	}
	q := h.tasks.questions[p.TaskID]
	if !q.Blocking || q.Recommendation != OptionApply || len(q.Answerers) != 1 {
		t.Errorf("question = %+v", q)
	}
	notes := h.tasks.messages[p.TaskID]
	if len(notes) != 3 || !strings.Contains(notes[2].Body, "new.internal") {
		t.Errorf("notes = %+v", notes)
	}

	// Unanswered: the next run neither re-asks nor re-judges.
	h.judge.calls = 0
	rep = h.run(t)
	if h.judge.calls != 0 || rep.Proposed != 0 || rep.OpenReviews != 1 {
		t.Fatalf("second run re-asked: calls=%d report=%+v", h.judge.calls, rep)
	}

	h.tasks.answer(p.TaskID, OptionApply)
	rep = h.run(t)
	if rep.Applied != 1 || h.mem.superseded["A"] != "Callers use new.internal:11434." {
		t.Fatalf("not applied: report=%+v superseded=%v", rep, h.mem.superseded)
	}
	if h.tasks.tasks[p.TaskID].Status != "done" || !strings.Contains(h.tasks.completed[p.TaskID], "Superseded memory A") {
		t.Errorf("task not completed: %+v %q", h.tasks.tasks[p.TaskID], h.tasks.completed[p.TaskID])
	}
	if _, open := h.st.Proposals["A"]; open {
		t.Error("proposal still open")
	}
	// The replacement is anchored at the review, so it starts from a clean slate.
	if r, ok := h.st.Reviews["NEW1"]; !ok || r.Outcome != "written_by_review" {
		t.Errorf("replacement review = %+v", h.st.Reviews["NEW1"])
	}
}

func TestOwnWordingAndKeepAndForget(t *testing.T) {
	h := newHarness(mem("A", "a uses x.internal"), mem("B", "b uses y.internal"), mem("C", "c uses z.internal"))
	for _, id := range []string{"A", "B"} {
		h.judge.verdicts[id] = judge.Verdict{Confidence: judge.High, Verdict: judge.NeedsUpdate, Reason: "r", Replacement: "proposed " + id}
	}
	h.judge.verdicts["C"] = judge.Verdict{Confidence: judge.High, Verdict: judge.Obsolete, Reason: "retired"}
	h.run(t)

	if opts := h.tasks.questions[h.st.Proposals["C"].TaskID].Options; len(opts) != 2 || opts[0] != OptionForget {
		t.Errorf("obsolete options = %v", opts)
	}
	h.tasks.answer(h.st.Proposals["A"].TaskID, "my own corrected text")
	h.tasks.answer(h.st.Proposals["B"].TaskID, OptionKeep)
	h.tasks.answer(h.st.Proposals["C"].TaskID, OptionForget)
	rep := h.run(t)

	if h.mem.superseded["A"] != "my own corrected text" {
		t.Errorf("A = %q", h.mem.superseded["A"])
	}
	if _, changed := h.mem.superseded["B"]; changed || h.st.Reviews["B"].Outcome != "kept" {
		t.Errorf("B changed or not recorded as kept: %+v", h.st.Reviews["B"])
	}
	if len(h.mem.forgotten) != 1 || h.mem.forgotten[0] != "C" {
		t.Errorf("forgotten = %v", h.mem.forgotten)
	}
	if rep.Applied != 2 || rep.OpenReviews != 0 {
		t.Errorf("report = %+v", rep)
	}

	// Kept memory B has the same findings next time: it is not asked again.
	h.judge.calls = 0
	h.run(t)
	if _, open := h.st.Proposals["B"]; open {
		t.Error("kept memory was asked about again")
	}
}

func TestMemoryChangedElsewhereClosesQuietly(t *testing.T) {
	h := newHarness(mem("A", "uses x.internal"))
	h.judge.verdicts["A"] = judge.Verdict{Confidence: judge.High, Verdict: judge.NeedsUpdate, Reason: "r", Replacement: "new"}
	h.run(t)
	taskID := h.st.Proposals["A"].TaskID

	delete(h.mem.live, "A") // Another agent superseded it meanwhile.
	h.tasks.answer(taskID, OptionApply)
	rep := h.run(t)
	if rep.Applied != 0 || h.st.Reviews["A"].Outcome != "changed_elsewhere" || h.tasks.tasks[taskID].Status != "done" {
		t.Fatalf("report=%+v review=%+v task=%+v", rep, h.st.Reviews["A"], h.tasks.tasks[taskID])
	}
}

func TestStillTrueIsRememberedAndJudgeOutageDefers(t *testing.T) {
	h := newHarness(mem("A", "uses x.internal"), mem("B", "uses y.internal"))
	h.judge.verdicts["A"] = judge.Verdict{Verdict: judge.StillTrue, Reason: "fine"}
	h.judge.verdicts["B"] = judge.Verdict{Verdict: judge.StillTrue, Reason: "fine"}
	h.run(t)
	if h.judge.calls != 2 {
		t.Fatalf("calls = %d", h.judge.calls)
	}
	h.judge.calls = 0
	if rep := h.run(t); h.judge.calls != 0 || rep.Flagged != 0 {
		t.Fatalf("re-judged unchanged findings: calls=%d report=%+v", h.judge.calls, rep)
	}

	h2 := newHarness(mem("A", "uses x.internal"))
	h2.judge.down = true
	rep := h2.run(t)
	if !rep.JudgeDown || rep.Deferred != 1 || len(h2.st.Reviews) != 0 || len(h2.st.Proposals) != 0 {
		t.Fatalf("report = %+v state = %+v", rep, h2.st)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(mem("A", "uses x.internal"))
	h.judge.verdicts["A"] = judge.Verdict{Confidence: judge.High, Verdict: judge.NeedsUpdate, Reason: "r", Replacement: "new"}
	h.g.DryRun = true
	rep := h.run(t)
	if rep.Proposed != 1 || len(h.tasks.tasks) != 0 || len(h.st.Proposals) != 0 || len(h.st.Reviews) != 0 {
		t.Fatalf("dry run wrote: report=%+v tasks=%d state=%+v", rep, len(h.tasks.tasks), h.st)
	}
}

func TestOpenProposalCap(t *testing.T) {
	h := newHarness(mem("A", "uses a.internal"), mem("B", "uses b.internal"), mem("C", "uses c.internal"))
	for _, id := range []string{"A", "B", "C"} {
		h.judge.verdicts[id] = judge.Verdict{Confidence: judge.High, Verdict: judge.Obsolete, Reason: "r"}
	}
	h.g.Policy.MaxOpenProposals = 2
	rep := h.run(t)
	if rep.Proposed != 2 || rep.Deferred != 1 || len(h.st.Proposals) != 2 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestChunkAndClip(t *testing.T) {
	s := strings.Repeat("é", 3000) // 6000 bytes, no newlines
	parts := chunk(s, 3900)
	if len(parts) != 2 || strings.Join(parts, "") != s {
		t.Fatalf("chunk lost data: %d parts", len(parts))
	}
	for _, p := range parts {
		if len(p) > 3900 || !strings.HasPrefix(p, "é") {
			t.Errorf("bad chunk boundary (len %d)", len(p))
		}
	}
	if c := clip(s, 10); len(c) > 10 || !strings.HasSuffix(c, "…") {
		t.Errorf("clip = %q", c)
	}
}

func TestLowConfidenceIsNotAsked(t *testing.T) {
	h := newHarness(mem("A", "uses x.internal"))
	h.judge.verdicts["A"] = judge.Verdict{Verdict: judge.NeedsUpdate, Confidence: judge.Medium, Reason: "maybe", Replacement: "new"}
	rep := h.run(t)
	if rep.Proposed != 0 || rep.Unsure != 1 || len(h.tasks.tasks) != 0 || h.st.Reviews["A"].Outcome != "judged_unsure" {
		t.Fatalf("report=%+v reviews=%+v", rep, h.st.Reviews)
	}
	h2 := newHarness(mem("A", "uses x.internal"))
	h2.judge.verdicts["A"] = judge.Verdict{Verdict: judge.NeedsUpdate, Confidence: judge.Medium, Reason: "maybe", Replacement: "new"}
	h2.g.Policy.MinConfidence = judge.Medium
	if rep := h2.run(t); rep.Proposed != 1 {
		t.Fatalf("medium threshold not honoured: %+v", rep)
	}
}
