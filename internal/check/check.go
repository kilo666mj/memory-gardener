// Package check turns a memory's references into staleness signals and the
// evidence a judge needs to decide whether the memory still holds.
package check

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/gitrepo"
	"github.com/kilo666mj/memory-gardener/internal/refs"
)

// Strength orders signals; only Medium and Strong ones go to the judge.
type Strength int

const (
	Weak Strength = iota + 1
	Medium
	Strong
)

func (s Strength) String() string {
	switch s {
	case Strong:
		return "strong"
	case Medium:
		return "medium"
	default:
		return "weak"
	}
}

// Signal is one mechanical reason to doubt a memory.
type Signal struct {
	Kind     string   `json:"kind"`
	Target   string   `json:"target"`
	Detail   string   `json:"detail"`
	Strength Strength `json:"strength"`
	// Version distinguishes repeat findings on the same target, such as the
	// newest commit that touched a path, so a reviewed finding resurfaces
	// only when something new happens.
	Version string `json:"version,omitempty"`
}

// Result is everything the checks learned about one memory.
type Result struct {
	Signals  []Signal
	Evidence []string
}

// Max is the strongest signal's strength, or 0 with no signals.
func (r Result) Max() Strength {
	var max Strength
	for _, s := range r.Signals {
		if s.Strength > max {
			max = s.Strength
		}
	}
	return max
}

// Fingerprint identifies the set of findings, for remembering decisions.
func (r Result) Fingerprint() string {
	keys := make([]string, 0, len(r.Signals))
	for _, s := range r.Signals {
		if s.Strength >= Medium {
			keys = append(keys, s.Kind+"|"+s.Target+"|"+s.Version)
		}
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:8])
}

// Repo is the part of a mirror the checks use; *gitrepo.Mirror implements it.
type Repo interface {
	Exists(ctx context.Context, path string) bool
	EverExisted(ctx context.Context, path string) bool
	LastRemoval(ctx context.Context, path string) string
	Base(ctx context.Context, cited []string, since time.Time) string
	ChangesSince(ctx context.Context, base, path string, limit int) ([]gitrepo.Commit, error)
	Diff(ctx context.Context, base, path string, max int) string
	Show(ctx context.Context, path string, max int) string
}

// Checker runs the mechanical checks.
type Checker struct {
	Repos            map[string]Repo
	Resolve          func(ctx context.Context, host string) error
	TransientAgeDays int
	Now              func() time.Time
	// EvidenceBudget bounds the evidence text per memory, in bytes.
	EvidenceBudget int
}

// DNSResolver resolves with the system resolver.
func DNSResolver(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := net.DefaultResolver.LookupHost(ctx, host)
	return err
}

// Check examines one memory's references. anchor is when the memory was last
// known to be true.
func (c *Checker) Check(ctx context.Context, r refs.Refs, anchor time.Time) Result {
	var res Result
	budget := c.EvidenceBudget
	if budget <= 0 {
		budget = 20000
	}
	add := func(text string) {
		if budget <= 0 || text == "" {
			return
		}
		if len(text) > budget {
			text = text[:budget] + "\n[evidence truncated]"
		}
		budget -= len(text)
		res.Evidence = append(res.Evidence, text)
	}

	for _, name := range r.Repos {
		repo, ok := c.Repos[name]
		if !ok {
			continue
		}
		if readme := repo.Show(ctx, "README.md", 1500); retired(readme) {
			res.Signals = append(res.Signals, Signal{Kind: "repo_retired", Target: name, Detail: "README marks the repository as retired or archived", Strength: Strong})
			add(fmt.Sprintf("README.md of %s (current):\n%s", name, readme))
		}
		base := repo.Base(ctx, r.Commits, anchor)
		for _, path := range r.Paths[name] {
			target := name + "/" + path
			if !repo.Exists(ctx, path) {
				if !repo.EverExisted(ctx, path) {
					continue // Not a path this repository ever had; probably elsewhere.
				}
				removal := repo.LastRemoval(ctx, path)
				res.Signals = append(res.Signals, Signal{Kind: "path_missing", Target: target, Detail: "no longer exists on the default branch", Strength: Strong})
				add(fmt.Sprintf("%s was deleted or renamed:\n%s", target, removal))
				continue
			}
			if base == "" {
				continue
			}
			commits, err := repo.ChangesSince(ctx, base, path, 20)
			if err != nil || len(commits) == 0 {
				continue
			}
			res.Signals = append(res.Signals, Signal{
				Kind: "path_changed", Target: target, Strength: Medium, Version: commits[0].Hash,
				Detail: fmt.Sprintf("%d commit(s) since %s, newest %s", len(commits), base, commits[0]),
			})
			add(fmt.Sprintf("Commits touching %s since %s:\n%s\n\nDiff %s..HEAD:\n%s",
				target, base, joinCommits(commits), base, repo.Diff(ctx, base, path, 6000)))
		}
		if base != "" && len(r.Transient) > 0 && c.age(anchor) >= c.TransientAgeDays {
			if commits, err := repo.ChangesSince(ctx, base, "", 30); err == nil && len(commits) > 0 {
				res.Signals = append(res.Signals, Signal{
					Kind: "transient_claim", Target: name, Strength: Medium, Version: commits[0].Hash,
					Detail: fmt.Sprintf("says %q, %d days old, %d commit(s) since", strings.Join(r.Transient, `", "`), c.age(anchor), len(commits)),
				})
				add(fmt.Sprintf("Commits in %s since %s:\n%s", name, base, joinCommits(commits)))
			}
		}
	}

	if len(r.Repos) == 0 && len(r.Transient) > 0 && c.age(anchor) >= c.TransientAgeDays {
		res.Signals = append(res.Signals, Signal{
			Kind: "transient_claim", Target: "memory", Strength: Weak,
			Detail: fmt.Sprintf("says %q and is %d days old", strings.Join(r.Transient, `", "`), c.age(anchor)),
		})
	}

	if c.Resolve != nil {
		for _, host := range r.Hosts {
			if err := c.Resolve(ctx, host); err != nil {
				res.Signals = append(res.Signals, Signal{Kind: "host_unresolvable", Target: host, Detail: err.Error(), Strength: Strong})
				add(fmt.Sprintf("DNS lookup of %s failed now: %v", host, err))
			}
		}
	}
	return res
}

func (c *Checker) age(anchor time.Time) int {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return int(now().Sub(anchor).Hours() / 24)
}

// retiredRe matches a README stating that this repository itself is retired,
// not one that merely mentions something else being retired.
var retiredRe = regexp.MustCompile(`(?im)^\s*(?:#+\s*.*\b(?:retired|archived|deprecated)\b|>?\s*\**\s*\[?(?:retired|archived|deprecated)\b)|\b(?:this (?:repository|repo|project|service|tool|module) (?:is|was|has been)(?: now)?|is now) (?:retired|archived|deprecated)\b|\bno longer (?:maintained|developed|deployed)\b`)

func retired(readme string) bool {
	lines := strings.Split(readme, "\n")
	if len(lines) > 15 {
		lines = lines[:15]
	}
	return retiredRe.MatchString(strings.Join(lines, "\n"))
}

func joinCommits(commits []gitrepo.Commit) string {
	lines := make([]string, len(commits))
	for i, c := range commits {
		lines[i] = c.String()
	}
	return strings.Join(lines, "\n")
}
