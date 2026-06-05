package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog"
)

// Operator wraps the local git CLI. All commands run in `workdir`.
type Operator struct {
	workdir string
	remote  string
	branch  string
	author  string
	email   string
	log     zerolog.Logger
}

func New(workdir, remote, branch, author, email string) *Operator {
	return NewWithLogger(workdir, remote, branch, author, email, zerolog.Nop())
}

func NewWithLogger(workdir, remote, branch, author, email string, log zerolog.Logger) *Operator {
	return &Operator{
		workdir: workdir,
		remote:  remote,
		branch:  branch,
		author:  author,
		email:   email,
		log:     log.With().Str("component", "gitops").Logger(),
	}
}

func (o *Operator) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = o.workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
	if err != nil {
		o.log.Debug().Strs("args", args).Str("output", combined).Err(err).Msg("git cmd failed")
		return combined, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, combined)
	}
	return combined, nil
}

// CommitAll stages everything under workdir and commits with the given
// message. Author/email are configured for the local repo only.
func (o *Operator) CommitAll(message string) error {
	if _, err := o.run("config", "user.name", o.author); err != nil {
		return err
	}
	if _, err := o.run("config", "user.email", o.email); err != nil {
		return err
	}
	if _, err := o.run("add", "-A"); err != nil {
		return err
	}
	if _, err := o.run("commit", "-m", message); err != nil {
		// "nothing to commit" should not be fatal
		if strings.Contains(err.Error(), "nothing to commit") || strings.Contains(err.Error(), "no changes added") {
			o.log.Debug().Msg("nothing to commit")
			return nil
		}
		return err
	}
	o.log.Info().Str("message", message).Msg("git commit")
	return nil
}

// SyncRebase pulls --rebase and pushes. On rejection it attempts
// fetch+rebase+push once more. Network errors are returned verbatim so
// the caller can surface them honestly.
func (o *Operator) SyncRebase() error {
	if _, err := o.run("pull", "--rebase", o.remote, o.branch); err != nil {
		o.log.Error().Err(err).Msg("git pull --rebase failed")
		return fmt.Errorf("pull: %w", err)
	}
	out, err := o.run("push", o.remote, o.branch)
	if err == nil {
		o.log.Info().Msg("git push ok")
		return nil
	}
	if !strings.Contains(err.Error(), "[rejected]") && !strings.Contains(out, "[rejected]") {
		o.log.Error().Err(err).Msg("git push failed")
		return fmt.Errorf("push: %w", err)
	}
	// Rejected — rebase onto remote and retry once.
	if _, ferr := o.run("fetch", o.remote); ferr != nil {
		o.log.Error().Err(ferr).Msg("git fetch failed")
		return fmt.Errorf("fetch: %w", ferr)
	}
	if _, rerr := o.run("rebase", o.remote+"/"+o.branch); rerr != nil {
		o.log.Error().Err(rerr).Msg("git rebase failed")
		return fmt.Errorf("rebase: %w", rerr)
	}
	if _, perr := o.run("push", o.remote, o.branch); perr != nil {
		o.log.Error().Err(perr).Msg("git push after rebase failed")
		return fmt.Errorf("push after rebase: %w", perr)
	}
	o.log.Info().Msg("git push ok after rebase")
	return nil
}

// Status is a thin wrapper around `git status --porcelain`.
func (o *Operator) Status() (string, error) {
	return o.run("status", "--porcelain")
}

// IsRepo returns true if workdir is inside a git repository.
func IsRepo(workdir string) bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = workdir
	return cmd.Run() == nil
}

// ErrNetwork is returned by SyncRebase when the underlying git process
// fails for connectivity reasons. The caller is expected to surface the
// failure honestly to the player.
var ErrNetwork = errors.New("git network operation failed")
