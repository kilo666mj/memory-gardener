package check

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/gitrepo"
	"github.com/kilo666mj/memory-gardener/internal/refs"
)

// fixture builds a source repository with dated commits and returns a mirror
// of it.
type fixture struct {
	t   *testing.T
	src string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := &fixture{t: t, src: t.TempDir()}
	f.git("", "init", "-q", "-b", "main")
	return f
}

func (f *fixture) git(date string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.src}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if date != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) commit(date, path, content, msg string) string {
	f.t.Helper()
	full := filepath.Join(f.src, path)
	if content == "" {
		f.git(date, "rm", "-q", path)
	} else {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
		f.git(date, "add", path)
	}
	f.git(date, "commit", "-q", "-m", msg)
	return f.git("", "rev-parse", "--short", "HEAD")
}

func (f *fixture) mirror() *gitrepo.Mirror {
	f.t.Helper()
	m, err := gitrepo.Sync(context.Background(), filepath.Join(f.t.TempDir(), "m.git"), f.src)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestPathChangedSinceCitedCommit(t *testing.T) {
	f := newFixture(t)
	cited := f.commit("2026-09-01T10:00:00Z", "internal/a.go", "package a\n// v1\n", "add a")
	f.commit("2026-09-10T10:00:00Z", "internal/a.go", "package a\n// v2\n", "rewrite a")
	f.commit("2026-09-11T10:00:00Z", "other.go", "package b\n", "unrelated")

	c := &Checker{Repos: map[string]Repo{"r": f.mirror()}}
	res := c.Check(context.Background(), refs.Refs{Repos: []string{"r"}, Paths: map[string][]string{"r": {"internal/a.go"}}, Commits: []string{cited}}, day("2026-09-01"))
	if len(res.Signals) != 1 || res.Signals[0].Kind != "path_changed" {
		t.Fatalf("signals = %+v", res.Signals)
	}
	if !strings.Contains(res.Signals[0].Detail, "rewrite a") || !strings.Contains(strings.Join(res.Evidence, ""), "+// v2") {
		t.Errorf("detail %q / evidence %q missing the change", res.Signals[0].Detail, res.Evidence)
	}
}

func TestUnchangedPathIsQuiet(t *testing.T) {
	f := newFixture(t)
	f.commit("2026-09-01T10:00:00Z", "internal/a.go", "package a\n", "add a")
	f.commit("2026-09-10T10:00:00Z", "other.go", "package b\n", "unrelated")

	c := &Checker{Repos: map[string]Repo{"r": f.mirror()}}
	res := c.Check(context.Background(), refs.Refs{Repos: []string{"r"}, Paths: map[string][]string{"r": {"internal/a.go"}}}, day("2026-09-05"))
	if len(res.Signals) != 0 {
		t.Fatalf("signals = %+v", res.Signals)
	}
}

func TestDeletedPathAndUnknownPath(t *testing.T) {
	f := newFixture(t)
	f.commit("2026-09-01T10:00:00Z", "lib/old.sh", "echo\n", "add old")
	f.commit("2026-09-02T10:00:00Z", "lib/old.sh", "", "drop old")

	c := &Checker{Repos: map[string]Repo{"r": f.mirror()}}
	res := c.Check(context.Background(), refs.Refs{Repos: []string{"r"}, Paths: map[string][]string{"r": {"lib/old.sh", "never/here.go"}}}, day("2026-09-01"))
	if len(res.Signals) != 1 || res.Signals[0].Kind != "path_missing" || res.Signals[0].Strength != Strong {
		t.Fatalf("signals = %+v", res.Signals)
	}
}

func TestRetiredRepo(t *testing.T) {
	f := newFixture(t)
	f.commit("2026-09-01T10:00:00Z", "README.md", "# Relay\n\n> **Retired on 8 October 2026.**\n", "retire")
	c := &Checker{Repos: map[string]Repo{"r": f.mirror()}}
	res := c.Check(context.Background(), refs.Refs{Repos: []string{"r"}, Paths: map[string][]string{}}, day("2026-09-01"))
	if len(res.Signals) != 1 || res.Signals[0].Kind != "repo_retired" {
		t.Fatalf("signals = %+v", res.Signals)
	}
}

func TestTransientClaimNeedsAgeAndActivity(t *testing.T) {
	f := newFixture(t)
	f.commit("2026-09-01T10:00:00Z", "a.go", "package a\n", "first")
	f.commit("2026-09-20T10:00:00Z", "b.go", "package b\n", "add remote")
	m := f.mirror()
	r := refs.Refs{Repos: []string{"r"}, Paths: map[string][]string{}, Transient: []string{"no remote yet"}}

	young := &Checker{Repos: map[string]Repo{"r": m}, TransientAgeDays: 7, Now: func() time.Time { return day("2026-09-03") }}
	if res := young.Check(context.Background(), r, day("2026-09-02")); len(res.Signals) != 0 {
		t.Fatalf("young memory flagged: %+v", res.Signals)
	}
	old := &Checker{Repos: map[string]Repo{"r": m}, TransientAgeDays: 7, Now: func() time.Time { return day("2026-10-09") }}
	res := old.Check(context.Background(), r, day("2026-09-02"))
	if len(res.Signals) != 1 || res.Signals[0].Kind != "transient_claim" || res.Signals[0].Strength != Medium {
		t.Fatalf("signals = %+v", res.Signals)
	}
}

func TestHostResolution(t *testing.T) {
	c := &Checker{Resolve: func(_ context.Context, host string) error {
		if host == "gone.internal" {
			return errors.New("no such host")
		}
		return nil
	}}
	res := c.Check(context.Background(), refs.Refs{Hosts: []string{"gone.internal", "here.internal"}}, day("2026-09-01"))
	if len(res.Signals) != 1 || res.Signals[0].Target != "gone.internal" {
		t.Fatalf("signals = %+v", res.Signals)
	}
}

func TestFingerprintIgnoresWeakSignalsAndOrder(t *testing.T) {
	a := Result{Signals: []Signal{{Kind: "x", Target: "1", Strength: Strong}, {Kind: "y", Target: "2", Strength: Medium, Version: "abc"}}}
	b := Result{Signals: []Signal{{Kind: "y", Target: "2", Strength: Medium, Version: "abc"}, {Kind: "w", Strength: Weak}, {Kind: "x", Target: "1", Strength: Strong}}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("fingerprints differ")
	}
	b.Signals[0].Version = "def"
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("a newer commit did not change the fingerprint")
	}
}

func TestRetiredDetection(t *testing.T) {
	for readme, want := range map[string]bool{
		"# Relay\n\n> **Retired on 8 October 2026.** Use Taskboard.":                      true,
		"# Old tool (archived)\n":                                                         true,
		"# x\n\nThis repository is deprecated in favour of y.":                            true,
		"# x\n\nThe service is no longer maintained.":                                     true,
		"# loginwatch\n\nIt replaces the `watch_login` feature of the retired `blocker`.": false,
		"# x\n\nRetries archived jobs.":                                                   false,
	} {
		if got := retired(readme); got != want {
			t.Errorf("retired(%q) = %v, want %v", readme, got, want)
		}
	}
}
