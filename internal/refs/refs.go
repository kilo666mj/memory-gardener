// Package refs extracts checkable references from a memory's text.
package refs

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kilo666mj/memory-gardener/internal/config"
)

// Refs is what a memory points at that can be checked against the world.
type Refs struct {
	// Repos are repository names from the policy.
	Repos []string
	// Paths are repository-relative file paths, keyed by repository name.
	// Paths found without a repository prefix are filed under every repo in
	// Repos when there is exactly one.
	Paths map[string][]string
	// Commits are hex strings that look like commit IDs.
	Commits []string
	// Hosts are host names ending in one of the policy's suffixes.
	Hosts []string
	// Transient lists phrases that only stay true for a while.
	Transient []string
	// Anchor is when the memory was last known true: an explicit
	// "Verified YYYY-MM-DD" date, otherwise its creation time.
	Anchor time.Time
}

var (
	verifiedRe = regexp.MustCompile(`(?i)\bverified(?: live| on| at)?[ :]+(\d{4}-\d{2}-\d{2})`)
	hexRe      = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	pathRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*/[A-Za-z0-9_-][A-Za-z0-9_.-]*\.[A-Za-z0-9]{1,8}$`)
	lineSuffix = regexp.MustCompile(`:\d+(?:-\d+)?$`)
	hostRe     = regexp.MustCompile(`\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+\b`)
)

// Extract finds references in content. scope is the memory's Wayminder scope.
func Extract(content, scope string, created time.Time, p config.Policy) Refs {
	r := Refs{Paths: map[string][]string{}, Anchor: created}
	if m := verifiedRe.FindStringSubmatch(content); m != nil {
		if t, err := time.Parse(time.DateOnly, m[1]); err == nil {
			r.Anchor = t
		}
	}

	repos := map[string]bool{}
	lower := strings.ToLower(content)
	for _, repo := range p.Repos {
		if scope == "repo:"+repo.Name {
			repos[repo.Name] = true
			continue
		}
		for _, alias := range repo.Aliases {
			if containsToken(lower, strings.ToLower(alias)) {
				repos[repo.Name] = true
			}
		}
		for _, prefix := range p.PathPrefixes {
			if strings.Contains(content, prefix+repo.Name+"/") || containsToken(content, prefix+repo.Name) {
				repos[repo.Name] = true
			}
		}
	}

	known := map[string]bool{}
	for _, repo := range p.Repos {
		known[repo.Name] = true
	}
	var loose []string
	for _, tok := range strings.Fields(content) {
		tok = strings.Trim(tok, "()[]{}<>\"'`,;!?")
		tok = strings.TrimSuffix(tok, ".")
		if strings.Contains(tok, "://") {
			continue
		}
		tok = lineSuffix.ReplaceAllString(tok, "")
		repo, rel, ok := repoPath(tok, p.PathPrefixes, known)
		switch {
		case ok:
			repos[repo] = true
			r.Paths[repo] = appendUnique(r.Paths[repo], rel)
		case strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") || strings.HasPrefix(tok, "."):
			// Host paths outside a known repository cannot be checked here.
		case pathRe.MatchString(tok):
			loose = appendUnique(loose, tok)
		}
	}
	for name := range repos {
		r.Repos = append(r.Repos, name)
	}
	sort.Strings(r.Repos)
	if len(r.Repos) == 1 {
		for _, rel := range loose {
			r.Paths[r.Repos[0]] = appendUnique(r.Paths[r.Repos[0]], rel)
		}
	}

	for _, h := range hexRe.FindAllString(content, -1) {
		if strings.ContainsAny(h, "abcdef") && strings.ContainsAny(h, "0123456789") {
			r.Commits = appendUnique(r.Commits, h)
		}
	}

	if len(p.HostSuffixes) > 0 {
		for _, h := range hostRe.FindAllString(lower, -1) {
			for _, suffix := range p.HostSuffixes {
				first, _, _ := strings.Cut(h, ".")
				if strings.HasSuffix(h, suffix) && h != strings.TrimPrefix(suffix, ".") && !slices.Contains(p.PlaceholderHosts, first) {
					r.Hosts = appendUnique(r.Hosts, h)
				}
			}
		}
	}

	for _, phrase := range p.TransientPhrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			r.Transient = append(r.Transient, phrase)
		}
	}
	return r
}

// repoPath splits a token such as "~/code/taskboard/internal/x.go" or
// "taskboard/internal/x.go" into a known repository and relative path.
func repoPath(tok string, prefixes []string, known map[string]bool) (string, string, bool) {
	for _, prefix := range prefixes {
		if rest, ok := strings.CutPrefix(tok, prefix); ok {
			tok = rest
			break
		}
	}
	repo, rel, ok := strings.Cut(tok, "/")
	if !ok || !known[repo] || !pathRe.MatchString(rel) {
		return "", "", false
	}
	return repo, rel, true
}

// containsToken reports whether needle appears in s with no word character
// directly on either side, so "taskboard" does not match "taskboard-dispatch".
func containsToken(s, needle string) bool {
	if needle == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(s[i:], needle)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(needle)
		if (start == 0 || !wordByte(s[start-1])) && (end == len(s) || !wordByte(s[end])) {
			return true
		}
		i = start + 1
	}
}

func wordByte(b byte) bool {
	return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
