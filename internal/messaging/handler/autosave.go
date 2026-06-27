package handler

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

// AutoSaveState holds the per-process reply counter and
// threshold used to trigger periodic git auto-saves. sysSt
// is the optional system_state.md writer — when non-nil,
// every successful save is recorded there (last_hash,
// total_saves counter) so the operator can confirm via a
// single file read what the bot has done.
type AutoSaveState struct {
	count     atomic.Int64
	threshold int
	sysSt     *usecase.SystemState
}

// NewAutoSaveState wires the per-process auto-save counter.
// A threshold <= 0 or a nil gitOp disables auto-save
// entirely (returns "" from MaybeAutoSave).
func NewAutoSaveState(cfg *config.Config, sysSt *usecase.SystemState) *AutoSaveState {
	return &AutoSaveState{
		threshold: max(cfg.Git.AutoSave.AfterMessages, 0),
		sysSt:     sysSt,
	}
}

// MaybeAutoSave increments the counter on every freeform
// reply (commands are excluded) and runs a git commit +
// push when the threshold is reached. Returns the
// notify text (empty when no save ran).
func (a *AutoSaveState) MaybeAutoSave(
	ctx context.Context,
	log zerolog.Logger,
	gitOp *gitops.Operator,
	_ string, // chatID kept for API symmetry with future per-chat throttling
	verbose bool,
) string {
	if a.threshold <= 0 || gitOp == nil {
		return ""
	}

	if n := a.count.Add(1); int(n)%a.threshold != 0 {
		return ""
	}

	return RunAutoSave(ctx, log, gitOp, a.sysSt, verbose)
}

// RunAutoSave commits (and pushes if not remote_disabled)
// and returns the player-facing notification text. An
// empty string means "nothing to say" (e.g. the commit was
// a no-op).
//
// The sysSt argument is optional: a nil value skips the
// system_state.md bookkeeping (used by tests). Production
// always passes a non-nil sysSt.
func RunAutoSave(
	ctx context.Context,
	log zerolog.Logger,
	op *gitops.Operator,
	sysSt *usecase.SystemState,
	verbose bool,
) string {
	if op == nil {
		return ""
	}

	res, err := op.CommitAll(ctx, "auto: save")
	if err != nil {
		log.Error().Err(err).Msg("auto-save commit failed")

		return "⚠️ auto-save: " + err.Error()
	}

	if res.Empty {
		return ""
	}

	// Record the autosave in system_state.md so the operator
	// can confirm "did the bot actually save" without
	// trawling zerolog. RecordAutosave is no-op for empty
	// commits (the counter reflects successful non-empty
	// saves, not save attempts).
	if sysSt != nil && res.Hash != "" {
		if _, err := sysSt.RecordAutosave(res.Hash, nowUTC()); err != nil {
			log.Warn().Err(err).Str("hash", res.Hash).Msg("system_state.md autosave record failed")
		}
	}

	body := buildAutoSaveNotify(res, verbose)
	if op.RemoteDisabled() {
		return body + "\n(push пропущен: remote_disabled=true)"
	}

	return appendPushStatus(ctx, log, op, res, body)
}

// buildAutoSaveNotify formats the ✅ "saved: commit <hash>"
// prefix (plus the optional per-file diff when verbose=true).
// The caller appends the push status separately so the
// function stays pure and testable.
func buildAutoSaveNotify(res gitops.CommitResult, verbose bool) string {
	var b strings.Builder

	b.WriteString("✅ сохранено: commit ")
	b.WriteString(res.Hash)

	if !verbose {
		return b.String()
	}

	b.WriteString("\n  файлов: ")
	b.WriteString(strconv.Itoa(len(res.FilesChanged)))

	for _, f := range res.FilesChanged {
		b.WriteString("\n  - ")
		b.WriteString(f)
	}

	return b.String()
}

// appendPushStatus tries to push the commit to the remote
// and appends a single line describing the result. Errors
// are reported inline so the player sees "⚠️ push: <reason>"
// rather than just an empty success line.
func appendPushStatus(ctx context.Context, log zerolog.Logger, op *gitops.Operator, res gitops.CommitResult, body string) string {
	if err := op.SyncRebase(ctx); err != nil {
		return body + "\n⚠️ push: " + err.Error()
	}

	log.Info().
		Str("hash", res.Hash).
		Int("files", len(res.FilesChanged)).
		Msg("auto-save pushed")

	return body + "\ngit push ok."
}
