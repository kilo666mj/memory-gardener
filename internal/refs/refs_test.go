package refs

import (
	"reflect"
	"testing"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/config"
)

func policy() config.Policy {
	return config.Policy{
		Repos: []config.Repo{
			{Name: "taskboard", Aliases: []string{"example/taskboard"}},
			{Name: "taskboard-dispatch", Aliases: []string{"example/taskboard-dispatch"}},
			{Name: "push-review"},
		},
		PathPrefixes:     []string{"~/code/"},
		HostSuffixes:     []string{".internal"},
		TransientPhrases: []string{"not yet", "no remote yet"},
	}
}

func TestExtract(t *testing.T) {
	created := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	content := "Verified 2026-09-27 against example/taskboard (internal/service/service.go canMutate at cff62bf). " +
		"Talks to memory.example.internal/mcp; see https://example.com/a/b.go. Not yet diagnosed."
	r := Extract(content, "personal", created, policy())

	if want := []string{"taskboard"}; !reflect.DeepEqual(r.Repos, want) {
		t.Errorf("Repos = %v, want %v", r.Repos, want)
	}
	if want := []string{"internal/service/service.go"}; !reflect.DeepEqual(r.Paths["taskboard"], want) {
		t.Errorf("Paths = %v, want %v", r.Paths, want)
	}
	if want := []string{"cff62bf"}; !reflect.DeepEqual(r.Commits, want) {
		t.Errorf("Commits = %v, want %v", r.Commits, want)
	}
	if want := []string{"memory.example.internal"}; !reflect.DeepEqual(r.Hosts, want) {
		t.Errorf("Hosts = %v, want %v", r.Hosts, want)
	}
	if want := []string{"not yet"}; !reflect.DeepEqual(r.Transient, want) {
		t.Errorf("Transient = %v, want %v", r.Transient, want)
	}
	if want := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC); !r.Anchor.Equal(want) {
		t.Errorf("Anchor = %v, want %v", r.Anchor, want)
	}
}

func TestExtractRepoForms(t *testing.T) {
	tests := []struct {
		name, content, scope string
		repos                []string
		paths                map[string][]string
	}{
		{"scope", "uses internal/x/y.go", "repo:push-review", []string{"push-review"}, map[string][]string{"push-review": {"internal/x/y.go"}}},
		{"workspace prefix", "edit ~/code/push-review/lib/pre-push.sh now", "personal", []string{"push-review"}, map[string][]string{"push-review": {"lib/pre-push.sh"}}},
		{"repo-relative path", "see taskboard/internal/model/model.go:671", "personal", []string{"taskboard"}, map[string][]string{"taskboard": {"internal/model/model.go"}}},
		{"alias is a whole token", "example/taskboard-dispatch is new", "personal", []string{"taskboard-dispatch"}, map[string][]string{}},
		{"loose paths need one repo", "example/taskboard and example/taskboard-dispatch share docs/a.md", "personal", []string{"taskboard", "taskboard-dispatch"}, map[string][]string{}},
		{"no repo", "docs/a.md changed", "personal", nil, map[string][]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Extract(tt.content, tt.scope, time.Now(), policy())
			if !reflect.DeepEqual(r.Repos, tt.repos) {
				t.Errorf("Repos = %v, want %v", r.Repos, tt.repos)
			}
			if !reflect.DeepEqual(r.Paths, tt.paths) {
				t.Errorf("Paths = %v, want %v", r.Paths, tt.paths)
			}
		})
	}
}

func TestExtractAnchorFallsBackToCreation(t *testing.T) {
	created := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if r := Extract("plain note", "personal", created, policy()); !r.Anchor.Equal(created) {
		t.Errorf("Anchor = %v, want %v", r.Anchor, created)
	}
	if r := Extract("Verified live 2026-09-29. x", "personal", created, policy()); r.Anchor.Format(time.DateOnly) != "2026-09-29" {
		t.Errorf("Anchor = %v, want 2026-09-29", r.Anchor)
	}
}

func TestCommitsNeedLettersAndDigits(t *testing.T) {
	r := Extract("ids 1234567 abcdefa 9472b6b96696b1024f9e579b2461746f2cfd4bf7", "personal", time.Now(), policy())
	if want := []string{"9472b6b96696b1024f9e579b2461746f2cfd4bf7"}; !reflect.DeepEqual(r.Commits, want) {
		t.Errorf("Commits = %v, want %v", r.Commits, want)
	}
}

func TestPlaceholderHostsIgnored(t *testing.T) {
	p := policy()
	p.PlaceholderHosts = []string{"host", "service"}
	r := Extract("ssh host.internal or service.internal, then real01.internal", "personal", time.Now(), p)
	if want := []string{"real01.internal"}; !reflect.DeepEqual(r.Hosts, want) {
		t.Errorf("Hosts = %v, want %v", r.Hosts, want)
	}
}
