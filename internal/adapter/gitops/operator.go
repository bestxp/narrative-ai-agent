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
	workdir        string
	remote         string
	branch         string
	author         string
	email          string
	remoteDisabled bool
	log            zerolog.Logger
}

func New(workdir, remote, branch, author, email string) *Operator {
	return NewWithLogger(workdir, remote, branch, author, email, false, zerolog.Nop())
}

// NewWithLogger is the explicit constructor: remoteDisabled flips
// the operator into local-only mode (no pull, no push, no fetch).
func NewWithLogger(workdir, remote, branch, author, email string, remoteDisabled bool, log zerolog.Logger) *Operator {
	return &Operator{
		workdir:        workdir,
		remote:         remote,
		branch:         branch,
		author:         author,
		email:          email,
		remoteDisabled: remoteDisabled,
		log:            log.With().Str("component", "gitops").Logger(),
	}
}

// RemoteDisabled reports whether the operator was configured to
// skip all remote operations. The dispatcher uses this to answer
// /push with a friendly "remote disabled" message.
func (o *Operator) RemoteDisabled() bool {
	return o.remoteDisabled
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

// CommitResult is the outcome of a successful CommitAll. The hash
// is the short SHA returned by `git rev-parse --short HEAD` after
// the commit; FilesChanged is the list of paths from
// `git show --name-only --pretty=format:`. An empty hash with
// Empty=true means "nothing to commit" — operators should not
// surface a saved notification in that case.
type CommitResult struct {
	Hash         string
	FilesChanged []string
	Empty        bool
}

// CommitAll stages everything under workdir and commits with the given
// message. Author/email are configured for the local repo only.
// The returned CommitResult is always non-nil; when there is
// nothing to commit Empty=true and the rest of the fields are
// zero. Network/permission errors are returned as the error
// value and CommitResult is the zero value.
func (o *Operator) CommitAll(message string) (CommitResult, error) {
	if _, err := o.run("config", "user.name", o.author); err != nil {
		return CommitResult{}, err
	}
	if _, err := o.run("config", "user.email", o.email); err != nil {
		return CommitResult{}, err
	}
	if _, err := o.run("add", "-A"); err != nil {
		return CommitResult{}, err
	}
	if _, err := o.run("commit", "-m", message); err != nil {
		// "nothing to commit" should not be fatal
		if strings.Contains(err.Error(), "nothing to commit") || strings.Contains(err.Error(), "no changes added") {
			o.log.Debug().Msg("nothing to commit")
			return CommitResult{Empty: true}, nil
		}
		return CommitResult{}, err
	}
	hash, herr := o.run("rev-parse", "--short", "HEAD")
	if herr != nil {
		// Commit happened, hash retrieval didn't — fall back to
		// the message in the log and an empty hash. The caller
		// still knows the commit succeeded.
		o.log.Warn().Err(herr).Msg("commit ok but hash retrieval failed")
	}
	files, _ := o.run("show", "--name-only", "--pretty=format:")
	res := CommitResult{Hash: strings.TrimSpace(hash)}
	for _, ln := range strings.Split(files, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			res.FilesChanged = append(res.FilesChanged, ln)
		}
	}
	o.log.Info().Str("message", message).Str("hash", res.Hash).Int("files", len(res.FilesChanged)).Msg("git commit")
	return res, nil
}

// ErrRemoteDisabled is returned by SyncRebase when the operator
// was constructed with remoteDisabled=true. The caller can detect
// it (or use RemoteDisabled()) and surface a friendly "remote
// disabled, commit only" reply to the player.
var ErrRemoteDisabled = errors.New("git remote is disabled — push skipped")

// SyncRebase pulls --rebase and pushes. On rejection it attempts
// fetch+rebase+push once more. Network errors are returned verbatim
// so the caller can surface them honestly. When the operator is in
// local-only mode (remoteDisabled=true) it short-circuits with
// ErrRemoteDisabled — no network call is made.
func (o *Operator) SyncRebase() error {
	if o.remoteDisabled {
		o.log.Info().Msg("git push skipped — remote disabled")
		return ErrRemoteDisabled
	}
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
