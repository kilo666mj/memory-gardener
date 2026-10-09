// Package config loads memory-gardener's environment and policy file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// Config is everything one gardening run needs.
type Config struct {
	DataDir string

	WayminderURL   string
	WayminderToken string

	TaskboardURL   string
	TaskboardToken string

	// Fleetglass is optional; an empty URL disables check publication.
	FleetglassURL   string
	FleetglassToken string
	FleetglassHost  string

	// JudgeURL is an OpenAI-compatible base URL, such as http://127.0.0.1:8080/v1.
	JudgeURL     string
	JudgeModel   string
	JudgeAPIKey  string
	JudgeTimeout time.Duration

	Policy Policy
}

// Policy is the JSON file that describes what to check and how much to ask.
type Policy struct {
	Repos []Repo `json:"repos"`
	// PathPrefixes are workspace locations whose next path segment is a
	// repository name, such as "~/code/".
	PathPrefixes []string `json:"path_prefixes"`
	// HostSuffixes limit DNS checks to names the gardener can judge, such as
	// ".internal". Empty disables host checks.
	HostSuffixes []string `json:"host_suffixes"`
	// PlaceholderHosts are first labels that mark an example rather than a
	// real host, such as "host" in "host.internal".
	PlaceholderHosts []string `json:"placeholder_hosts"`
	// TransientPhrases mark claims that are only true for a while ("not yet").
	TransientPhrases []string `json:"transient_phrases"`
	TransientAgeDays int      `json:"transient_age_days"`

	MaxJudgementsPerRun int `json:"max_judgements_per_run"`
	// MinConfidence is the lowest model confidence (low, medium, high) that
	// may turn into a question for a person.
	MinConfidence string `json:"min_confidence"`
	// MaxOpenProposals caps how many review tasks may wait on a person at once.
	MaxOpenProposals int `json:"max_open_proposals"`

	Task TaskPolicy `json:"task"`
}

// Repo is a git repository memories may refer to.
type Repo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Aliases are extra strings that identify the repository, in addition to
	// "repo:<name>" scopes and the "<owner>/<name>" derived from URL.
	Aliases []string `json:"aliases"`
}

// TaskPolicy shapes the Taskboard review tasks.
type TaskPolicy struct {
	Project   string   `json:"project"`
	Section   string   `json:"section"`
	Priority  string   `json:"priority"`
	Answerers []string `json:"answerers"`
}

// Load reads the environment, then the policy file it names.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DataDir:         or(getenv("MEMORY_GARDENER_DATA_DIR"), "/var/lib/memory-gardener"),
		WayminderURL:    getenv("MEMORY_GARDENER_WAYMINDER_URL"),
		WayminderToken:  getenv("MEMORY_GARDENER_WAYMINDER_TOKEN"),
		TaskboardURL:    strings.TrimRight(getenv("MEMORY_GARDENER_TASKBOARD_URL"), "/"),
		TaskboardToken:  getenv("MEMORY_GARDENER_TASKBOARD_TOKEN"),
		FleetglassURL:   strings.TrimRight(getenv("MEMORY_GARDENER_FLEETGLASS_URL"), "/"),
		FleetglassToken: getenv("MEMORY_GARDENER_FLEETGLASS_TOKEN"),
		FleetglassHost:  getenv("MEMORY_GARDENER_FLEETGLASS_HOST"),
		JudgeURL:        strings.TrimRight(or(getenv("MEMORY_GARDENER_JUDGE_URL"), "http://127.0.0.1:8080/v1"), "/"),
		JudgeModel:      getenv("MEMORY_GARDENER_JUDGE_MODEL"),
		JudgeAPIKey:     getenv("MEMORY_GARDENER_JUDGE_API_KEY"),
		JudgeTimeout:    5 * time.Minute,
	}
	if v := getenv("MEMORY_GARDENER_JUDGE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("MEMORY_GARDENER_JUDGE_TIMEOUT: %w", err)
		}
		cfg.JudgeTimeout = d
	}
	if cfg.FleetglassHost == "" {
		cfg.FleetglassHost, _ = os.Hostname()
	}
	if cfg.WayminderURL == "" || cfg.WayminderToken == "" {
		return Config{}, errors.New("MEMORY_GARDENER_WAYMINDER_URL and MEMORY_GARDENER_WAYMINDER_TOKEN are required")
	}

	policyPath := or(getenv("MEMORY_GARDENER_POLICY"), "/etc/memory-gardener/policy.json")
	policy, err := LoadPolicy(policyPath)
	if err != nil {
		return Config{}, err
	}
	cfg.Policy = policy
	return cfg, nil
}

// LoadPolicy reads and validates a policy file, filling defaults.
func LoadPolicy(file string) (Policy, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Policy{}, fmt.Errorf("read policy: %w", err)
	}
	var p Policy
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("parse policy %s: %w", file, err)
	}
	return p, p.normalize()
}

func (p *Policy) normalize() error {
	seen := map[string]bool{}
	for i, r := range p.Repos {
		if r.Name == "" || r.URL == "" {
			return fmt.Errorf("policy repos[%d]: name and url are required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("policy repos: duplicate name %q", r.Name)
		}
		seen[r.Name] = true
		if owner := ownerName(r.URL); owner != "" {
			p.Repos[i].Aliases = append(p.Repos[i].Aliases, owner)
		}
	}
	if p.TransientPhrases == nil {
		p.TransientPhrases = []string{"not yet", "no remote yet", "not diagnosed", "work in progress", "for now", "temporarily"}
	}
	if p.PlaceholderHosts == nil {
		p.PlaceholderHosts = []string{"host", "hostname", "service", "guest", "node", "server", "name", "example", "app", "foo", "bar", "myhost", "your-host"}
	}
	if p.TransientAgeDays <= 0 {
		p.TransientAgeDays = 7
	}
	if p.MaxJudgementsPerRun <= 0 {
		p.MaxJudgementsPerRun = 20
	}
	switch p.MinConfidence {
	case "":
		p.MinConfidence = "high"
	case "low", "medium", "high":
	default:
		return fmt.Errorf("policy min_confidence: %q is not low, medium or high", p.MinConfidence)
	}
	if p.MaxOpenProposals <= 0 {
		p.MaxOpenProposals = 10
	}
	if p.Task.Project == "" {
		p.Task.Project = "memory-gardener"
	}
	if p.Task.Priority == "" {
		p.Task.Priority = "low"
	}
	return nil
}

// ownerName returns "owner/name" for a git URL, without a .git suffix.
func ownerName(raw string) string {
	var p string
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		p = u.Path
	} else if i := strings.Index(raw, ":"); i > 0 { // scp-like git@host:owner/name
		p = raw[i+1:]
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	dir, name := path.Split(p)
	owner := path.Base(strings.TrimSuffix(dir, "/"))
	if owner == "" || owner == "." || owner == "/" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func or(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
