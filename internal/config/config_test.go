package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerName(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/example/taskboard.git": "example/taskboard",
		"https://git.example.com/team/repo":        "team/repo",
		"git@github.com:example/wayminder.git":     "example/wayminder",
		"/srv/git/solo.git":                        "",
		"nonsense":                                 "",
	} {
		if got := ownerName(in); got != want {
			t.Errorf("ownerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadPolicyDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"repos":[{"name":"taskboard","url":"https://github.com/example/taskboard.git"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(good)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Repos[0].Aliases; len(got) != 1 || got[0] != "example/taskboard" {
		t.Errorf("aliases = %v", got)
	}
	if p.MaxOpenProposals != 10 || p.MaxJudgementsPerRun != 20 || p.TransientAgeDays != 7 || p.Task.Priority != "low" || len(p.TransientPhrases) == 0 {
		t.Errorf("defaults not applied: %+v", p)
	}

	for name, body := range map[string]string{
		"unknown field": `{"repoz":[]}`,
		"missing url":   `{"repos":[{"name":"x"}]}`,
		"duplicate":     `{"repos":[{"name":"x","url":"u"},{"name":"x","url":"v"}]}`,
	} {
		file := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPolicy(file); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestLoadRequiresWayminder(t *testing.T) {
	env := map[string]string{}
	if _, err := Load(func(k string) string { return env[k] }); err == nil {
		t.Fatal("no error without Wayminder settings")
	}
}
