// Package gitrepo keeps bare mirrors of policy repositories and answers the
// questions the checks ask of them.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Mirror is a bare clone of one repository.
type Mirror struct {
	Dir string
}

// Sync clones url into dir as a bare mirror, or fetches if it exists.
func Sync(ctx context.Context, dir, url string) (*Mirror, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if _, err := git(ctx, dir, "remote", "set-url", "origin", url); err != nil {
			return nil, err
		}
		if _, err := git(ctx, dir, "fetch", "--prune", "--quiet", "origin"); err != nil {
			return nil, err
		}
		// A mirror's HEAD can go stale if the default branch was renamed.
		if _, err := git(ctx, dir, "remote", "set-head", "origin", "--auto"); err == nil {
			if ref, err := git(ctx, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
				branch := strings.TrimPrefix(ref, "refs/remotes/origin/")
				_, _ = git(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/"+branch)
			}
		}
		return &Mirror{Dir: dir}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, err
	}
	if _, err := git(ctx, "", "clone", "--mirror", "--quiet", url, dir); err != nil {
		return nil, err
	}
	return &Mirror{Dir: dir}, nil
}

// Commit is one log entry.
type Commit struct {
	Hash    string
	Date    string
	Subject string
}

func (c Commit) String() string { return c.Hash + " " + c.Date + " " + c.Subject }

// HasCommit reports whether rev names a commit in the mirror.
func (m *Mirror) HasCommit(ctx context.Context, rev string) bool {
	_, err := git(ctx, m.Dir, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

// Exists reports whether path exists on the default branch.
func (m *Mirror) Exists(ctx context.Context, path string) bool {
	_, err := git(ctx, m.Dir, "cat-file", "-e", "HEAD:"+path)
	return err == nil
}

// EverExisted reports whether path appears anywhere in the default branch's
// history, which separates "deleted or moved" from "never a real path".
func (m *Mirror) EverExisted(ctx context.Context, path string) bool {
	out, err := git(ctx, m.Dir, "log", "-1", "--format=%h", "HEAD", "--", path)
	return err == nil && out != ""
}

// LastRemoval describes the commit that deleted or renamed path, if any.
func (m *Mirror) LastRemoval(ctx context.Context, path string) string {
	out, err := git(ctx, m.Dir, "log", "-1", "--format=%h %cs %s", "--name-status", "--diff-filter=DR", "-M", "HEAD", "--", path)
	if err != nil {
		return ""
	}
	return out
}

// Base resolves the commit to compare against: the cited commit when the
// mirror has it, otherwise the last commit on the default branch before since.
// It returns "" when the repository has no history before since.
func (m *Mirror) Base(ctx context.Context, cited []string, since time.Time) string {
	for _, c := range cited {
		if m.HasCommit(ctx, c) {
			if out, err := git(ctx, m.Dir, "rev-parse", "--short=12", c+"^{commit}"); err == nil {
				return out
			}
		}
	}
	out, err := git(ctx, m.Dir, "rev-list", "-1", "--abbrev-commit", "--abbrev=12", "--before="+since.Format(time.RFC3339), "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// ChangesSince lists default-branch commits after base that touch path
// (or the whole repository when path is empty), newest first.
func (m *Mirror) ChangesSince(ctx context.Context, base, path string, limit int) ([]Commit, error) {
	args := []string{"log", fmt.Sprintf("-%d", limit), "--format=%h%x09%cs%x09%s", base + "..HEAD"}
	if path != "" {
		args = append(args, "--", path)
	}
	out, err := git(ctx, m.Dir, args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		if parts := strings.SplitN(line, "\t", 3); len(parts) == 3 {
			commits = append(commits, Commit{Hash: parts[0], Date: parts[1], Subject: parts[2]})
		}
	}
	return commits, nil
}

// Diff returns the diff of path between base and the default branch,
// truncated to max bytes.
func (m *Mirror) Diff(ctx context.Context, base, path string, max int) string {
	out, err := git(ctx, m.Dir, "diff", "--no-color", "-U2", base+"..HEAD", "--", path)
	if err != nil {
		return ""
	}
	return truncate(out, max)
}

// Show returns a file from the default branch, truncated to max bytes.
func (m *Mirror) Show(ctx context.Context, path string, max int) string {
	out, err := git(ctx, m.Dir, "show", "HEAD:"+path)
	if err != nil {
		return ""
	}
	return truncate(out, max)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n[truncated]"
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
