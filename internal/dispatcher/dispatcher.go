// Package dispatcher routes incoming messages from any
// messaging.Client to the GM usecase. It is transport-agnostic; the
// old hard-coded /command set lives here as a thin handler.
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bestxp/narrative-ai-agent/internal/adapter/gitops"
	"github.com/bestxp/narrative-ai-agent/internal/adapter/storage"
	"github.com/bestxp/narrative-ai-agent/internal/config"
	"github.com/bestxp/narrative-ai-agent/internal/domain"
	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/slowlog"
	"github.com/bestxp/narrative-ai-agent/internal/structured"
	"github.com/bestxp/narrative-ai-agent/internal/usecase"
	"github.com/rs/zerolog"
)

// Dispatcher turns an IncomingMessage into a reply string. It is the
// single point that knows about /commands; everything else flows
// through the GM.
type Dispatcher struct {
	cfg   *config.Config
	fs    *storage.FileStore
	git   *gitops.Operator
	rf    *usecase.ResponseFormat
	fl    *usecase.FirstLaunch
	ss    *usecase.SessionStart
	tools usecase.Tool
	gm    *usecase.GM // optional; nil until main.go wires the LLM
	slow  *slowlog.Logger
	log   zerolog.Logger
}

func New(
	cfg *config.Config,
	fs *storage.FileStore,
	git *gitops.Operator,
	tools usecase.Tool,
	slow *slowlog.Logger,
	log zerolog.Logger,
) *Dispatcher {
	l := log.With().Str("component", "dispatcher").Logger()

	return &Dispatcher{
		cfg:   cfg,
		fs:    fs,
		git:   git,
		rf:    usecase.NewResponseFormat(cfg.Narrative.WordLimit, cfg.Narrative.Language),
		fl:    usecase.NewFirstLaunch(fs),
		ss:    usecase.NewSessionStart(fs),
		tools: tools,
		slow:  slow,
		log:   l,
	}
}

// AttachGM plugs the LLM-driven GM into the dispatcher. Calling this
// after New is the canonical wiring path; tests can leave GM nil and
// the freeform handler falls back to echo+validate.
func (d *Dispatcher) AttachGM(gm *usecase.GM) {
	d.gm = gm
}

// Handle is the central entry point. It returns the reply body; the
// caller (MultiClient dispatcher goroutine) is responsible for
// sending and, when applicable, opening a stream.
//
// For freeform messages Handle internally calls HandleStream with a
// buffering callback. Callers that want to show the LLM "thinking"
// status to the player should call HandleStream directly with their
// own OnStatus / OnDelta callbacks.
func (d *Dispatcher) Handle(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	d.log.Info().Str("chat", msg.ChatID).Str("command", msg.Command).Strs("args", msg.Args).Msg("dispatch")

	if msg.Command == "" {
		var buf strings.Builder

		cb := usecase.Callbacks{OnDelta: func(s string) error {
			buf.WriteString(s)

			return nil
		}}
		if err := d.HandleStream(ctx, msg, cb); err != nil {
			return "", err
		}

		return buf.String(), nil
	}

	switch msg.Command {
	case "start":
		return d.cmdStart()
	case "launch":
		return d.cmdLaunch(ctx, msg)
	case "status":
		return d.cmdStatus()
	case "me":
		return d.cmdMe()
	case "commit":
		return d.cmdCommit(ctx, msg)
	case "save":
		return d.cmdSave(ctx, d.cfg.Git.VerboseSave)
	case "push":
		return d.cmdPush(ctx)
	case "maintenance":
		return d.cmdMaintenance(ctx)
	case "endday":
		return d.cmdEndDay(ctx, msg)
	case "leave":
		return d.cmdLeave(ctx, msg)
	case "return":
		return d.cmdReturn(ctx, msg)
	case "reload":
		return d.cmdReload()
	case "help":
		return d.cmdHelp()
	}

	return "", nil
}

// HandleStream is the callback-based entry point. It is used by
// HandleStream is the callback-based entry point. It is used by
// main.go to drive Telegram streaming — OnDelta is called for
// every LLM text fragment, OnStatus rotates the "…" placeholder
// into an informative phase label, and OnTokens is called with
// the cumulative token usage once per LLM round. All callbacks
// are optional; the function is a no-op when no GM is wired.
func (d *Dispatcher) HandleStream(ctx context.Context, msg messaging.IncomingMessage, cb usecase.Callbacks) error {
	if msg.Command != "" {
		// Commands are non-streaming. Buffer and return.
		reply, err := d.Handle(ctx, msg)
		if err != nil {
			return fmt.Errorf("handle_stream: Handle failed: %w", err)
		}

		if cb.OnDelta != nil && reply != "" {
			if err := cb.OnDelta(reply); err != nil {
				return fmt.Errorf("handle_stream: OnDelta failed: %w", err)
			}
		}

		return nil
	}

	if d.gm == nil {
		// Fallback: echo + validate, sent as a single chunk.
		v := d.rf.Validate(msg.Text)

		reply := fmt.Sprintf(
			structured.HeaderDialogue+"\n%s\n\n"+
				structured.HeaderContext+"\nбез изменений\n\n"+
				structured.HeaderValidation+"\n"+
				"- Лимит слов: %d\n"+
				"- Слов в ответе: %d\n"+
				"- Превышен: %v\n"+
				"- Запрещённые формы: %v\n",
			msg.Text, d.cfg.Narrative.WordLimit, v.WordCount, v.OverLimit, v.ForbiddenForms)
		if cb.OnDelta != nil {
			if err := cb.OnDelta(reply); err != nil {
				return fmt.Errorf("handle_stream: OnDelta failed: %w", err)
			}
		}

		return nil
	}

	if d.slow != nil {
		_ = d.slow.Write("incoming.text", msg.ChatID, map[string]any{
			"sender":  msg.Sender.ID,
			"text":    msg.Text,
			"command": msg.Command,
		})
	}

	_, err := d.gm.Reply(ctx, msg.ChatID, msg.Text, cb)
	if err != nil {
		return fmt.Errorf("handle_stream: Reply failed: %w", err)
	}

	return nil
}

// ResendLast re-runs the GM on the last user message of the given
// chat, discarding the previous LLM answer. The new answer streams
// through cb the same way a normal HandleStream reply does. Returns
// usecase.ErrNoLastUserTurn when the chat has no history yet.
//
// This is the dispatcher entry point for the "resend last" UX in
// the wschat transport; it is transport-agnostic so any future
// client (Telegram inline button, Discord slash command) can drive
// the same path.
func (d *Dispatcher) ResendLast(ctx context.Context, chatID string, cb usecase.Callbacks) error {
	if d.gm == nil {
		return errors.New("gm not wired")
	}

	_, err := d.gm.RegenerateLast(ctx, chatID, cb)
	if err != nil {
		return fmt.Errorf("dispatcher.ResendLast: %w", err)
	}

	return nil
}

// EditLast replaces the last user message in the given chat with
// newText and re-runs the GM, discarding the previous LLM answer.
// The new answer streams through cb. Returns
// usecase.ErrNoLastUserTurn when the chat has no user message to
// edit.
func (d *Dispatcher) EditLast(ctx context.Context, chatID, newText string, cb usecase.Callbacks) error {
	if d.gm == nil {
		return errors.New("gm not wired")
	}

	_, err := d.gm.EditAndRegenerate(ctx, chatID, newText, cb)
	if err != nil {
		return fmt.Errorf("dispatcher.EditLast: %w", err)
	}

	return nil
}

// Commands returns the canonical command set with short
// descriptions, suitable for handing to a transport's
// SetCommands. The list is the source of truth — main.go
// uses it for the Telegram native menu, but any transport
// that supports a command picker can consume the same
// slice. Keep the descriptions short (≤256 chars per
// Telegram).
func (d *Dispatcher) Commands() []messaging.BotCommand {
	return []messaging.BotCommand{
		{Command: "start", Description: "Загрузить info.yaml и world state"},
		{Command: "status", Description: "Текущий персонаж, мир, день"},
		{Command: "me", Description: "Содержимое SOUL/SKILL/memory/state"},
		{Command: "launch", Description: "Первоначальная настройка (перс + мир)"},
		{Command: "endday", Description: "Записать день в chronicle"},
		{Command: "maintenance", Description: "Сжать NPC > 40 строк"},
		{Command: "leave", Description: "Переход в новый мир"},
		{Command: "return", Description: "Возврат с time-skip"},
		{Command: "reload", Description: "Перечитать данные активного мира из источника"},
		{Command: "save", Description: "git commit + push, уведомление"},
		{Command: "commit", Description: "Только git commit (локально)"},
		{Command: "push", Description: "Только git push"},
		{Command: "help", Description: "Список команд"},
	}
}

func (d *Dispatcher) commit(ctx context.Context, msg string) {
	if d.git == nil {
		return
	}

	_, _ = d.git.CommitAll(ctx, msg)
}

func (d *Dispatcher) cmdStart() (string, error) {
	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активной сессии. Используйте /launch <персонаж> <мир> для первоначальной настройки.", nil
	}

	sc, err := d.ss.Start()
	if err != nil {
		return "", fmt.Errorf("cmd_start: Start failed: %w", err)
	}

	warn := ""
	if len(sc.Warnings) > 0 {
		warn = "\nПредупреждения:\n- " + strings.Join(sc.Warnings, "\n- ")
	}

	if sc.SyncChronicleAhead {
		warn += "\n⚠️ chronicle опережает world state — галлюцинация? Спросить игрока."
	}

	if sc.SyncStateAhead {
		warn += "\n⚠️ world state опережает chronicle — дописать недостающие дни."
	}

	return fmt.Sprintf("**Сессия запущена**\nПерсонаж: %s\nМир: %s\n\n**world state**\n%s%s",
		sc.Character, sc.World, sc.State, warn), nil
}

func (d *Dispatcher) cmdLaunch(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	if len(msg.Args) < 2 {
		return "Использование: /launch <имя_перса> <имя_мира> [канон/сценарий...]", nil
	}

	charName := msg.Args[0]
	worldName := msg.Args[1]
	canon := strings.Join(msg.Args[2:], " ")

	err := d.fl.Launch(
		usecase.CharacterSpec{DisplayName: charName, Dir: charName, TrueNature: "(опишите позже)", Philosophy: ""},
		usecase.WorldSpec{DisplayName: worldName, Dir: worldName, IsKnown: false, Canon: canon},
	)
	if err != nil {
		if errors.Is(err, usecase.ErrAlreadyLaunched) {
			return "Сессия уже инициализирована. Используйте /status для просмотра.", nil
		}

		return "", fmt.Errorf("dispatcher.cmdLaunch: %w", err)
	}

	d.commit(ctx, "first launch: "+charName+" / "+worldName)

	return fmt.Sprintf("**Создано**\nПерсонаж: %s\nМир: %s\nИспользуйте /start для просмотра world state.", charName, worldName), nil
}

func (d *Dispatcher) cmdStatus() (string, error) {
	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активной сессии. /launch сначала.", nil
	}

	sc, err := d.ss.Start()
	if err != nil {
		return "", fmt.Errorf("cmd_status: Start failed: %w", err)
	}

	return fmt.Sprintf("Персонаж: %s\nМир: %s\n\n**world state**\n%s", sc.Character, sc.World, sc.State), nil
}

// cmdMe shows the active character's YAML files (SOUL.yaml,
// skill.yaml, memory.yaml, inventory.yaml) and the current state.
// Each section is truncated to roughly the screen size so a
// Telegram reply stays under 4096 chars even on heavily-developed
// characters.
func (d *Dispatcher) cmdMe() (string, error) {
	if d.tools == nil {
		return "character_update не подключён.", nil
	}

	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активной сессии. /launch сначала.", nil
	}

	raw, _ := d.fs.ReadRaw(storage.InfoFile)

	parsed, parseErr := domain.ParseInfo(raw)
	if parseErr == nil && parsed.ActiveCharacter == "" {
		return "Нет активного персонажа. /launch сначала.", nil
	}

	if parseErr != nil {
		return "", fmt.Errorf("parse info: %w", parseErr)
	}

	snap, err := d.tools.Read(parsed.ActiveCharacter, parsed.ActiveWorld)
	if err != nil {
		return "", fmt.Errorf("wrap: %w", err)
	}

	return usecase.FormatCharacterSnapshot(snap, 40), nil
}

func (d *Dispatcher) cmdCommit(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	if d.git == nil {
		return "git не инициализирован (тестовый режим).", nil
	}

	commitMsg := "auto: maintenance"
	if len(msg.Args) > 0 {
		commitMsg = strings.Join(msg.Args, " ")
	}

	res, err := d.git.CommitAll(ctx, commitMsg)
	if err != nil {
		return "", fmt.Errorf("cmd_commit: CommitAll failed: %w", err)
	}

	return d.formatCommitResult(res, false), nil
}

// cmdSave is the explicit "commit + push" entry point. It returns
// a one-line summary (or a multi-line block when verbose=true);
// the caller (main.go) decides whether to surface it as a
// separate Telegram message or fold it into the next reply.
func (d *Dispatcher) cmdSave(ctx context.Context, verbose bool) (string, error) {
	if d.git == nil {
		return "git не инициализирован (тестовый режим).", nil
	}

	res, err := d.git.CommitAll(ctx, "auto: save")
	if err != nil {
		return "", fmt.Errorf("cmd_save: CommitAll failed: %w", err)
	}

	body := d.formatCommitResult(res, verbose)
	if d.git.RemoteDisabled() || res.Empty {
		return body, nil
	}

	if err := d.git.SyncRebase(ctx); err != nil {
		if errors.Is(err, gitops.ErrRemoteDisabled) {
			return body, nil
		}

		return body + "\n⚠️ push: " + err.Error(), nil
	}

	return body + "\ngit push ok.", nil
}

// formatCommitResult renders the CommitResult for the player.
// Empty commits get a one-liner saying so (no notification
// should really fire for an empty commit). Real commits return
// either a short "✅ сохранено: commit abc1234" or a verbose
// block listing the touched files.
func (d *Dispatcher) formatCommitResult(res gitops.CommitResult, verbose bool) string {
	if res.Empty {
		return "нечего коммитить (no changes)."
	}

	if verbose {
		var b strings.Builder
		b.WriteString("✅ сохранено: commit ")
		b.WriteString(res.Hash)
		b.WriteString("\n  файлов: ")
		b.WriteString(strconv.Itoa(len(res.FilesChanged)))
		b.WriteString("\n")

		for _, f := range res.FilesChanged {
			b.WriteString("  - ")
			b.WriteString(f)
			b.WriteString("\n")
		}

		return b.String()
	}

	return "✅ сохранено: commit " + res.Hash
}

func (d *Dispatcher) cmdPush(ctx context.Context) (string, error) {
	if d.git == nil {
		return "git не инициализирован.", nil
	}

	if d.git.RemoteDisabled() {
		return "git push пропущен: remote_disabled=true (только локальные коммиты).", nil
	}

	if err := d.git.SyncRebase(ctx); err != nil {
		return "", fmt.Errorf("cmd_push: %w", err)
	}

	return "git push выполнен.", nil
}

func (d *Dispatcher) cmdMaintenance(ctx context.Context) (string, error) {
	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активного мира.", nil
	}

	sc, err := d.ss.Start()
	if err != nil {
		return "", fmt.Errorf("cmd_maintenance: Start failed: %w", err)
	}

	touched, err := d.tools.MaintainNPCs(sc.World)
	if err != nil {
		return "", fmt.Errorf("cmd_maintenance: MaintainNPCs failed: %w", err)
	}

	d.commit(ctx, "maintenance: "+sc.World)
	// Lore is compacted in the same /maintenance call
	// so an operator can run the bot's daily cleanup
	// with one command. Both paths are LLM-driven and
	// best-effort — a failed summarizer call is logged
	// and skipped, not surfaced to the operator as an
	// error. canon.md is NEVER touched here. We use
	// context.Background (not a request-scoped ctx)
	// because /maintenance is operator-triggered and
	// may legitimately take a minute or two for a
	// large lore.md.
	_, loreErr := d.tools.MaintainLore(ctx, sc.World)
	if loreErr != nil {
		d.log.Warn().Err(loreErr).Msg("lore maintenance failed")
	}

	if len(touched) > 0 {
		return "Обслуживание выполнено. Выжимка NPC: " + strings.Join(touched, ", "), nil
	}

	return "Обслуживание выполнено. Выжимка NPC: не требуется.", nil
}

func (d *Dispatcher) cmdEndDay(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	if len(msg.Args) < 2 {
		return "Использование: /endday <номер_дня> <краткая_выжимка...>", nil
	}

	day, err := strconv.Atoi(msg.Args[0])
	if err != nil {
		return "", fmt.Errorf("day must be int: %w", err)
	}

	summary := strings.Join(msg.Args[1:], " ")

	sc, err := d.ss.Start()
	if err != nil {
		return "", fmt.Errorf("cmd_archive: Start failed: %w", err)
	}

	if err := d.tools.ArchiveChronicleDay(ctx, sc.World, day, summary); err != nil {
		return "", fmt.Errorf("wrap: %w", err)
	}

	d.commit(ctx, fmt.Sprintf("День %d", day))

	return fmt.Sprintf("День %d заархивирован в chronicle.", day), nil
}

func (d *Dispatcher) cmdLeave(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	if len(msg.Args) < 1 {
		return "Использование: /leave <новый_мир> [прошло_времени]", nil
	}

	to := msg.Args[0]

	skip := ""
	if len(msg.Args) > 1 {
		skip = strings.Join(msg.Args[1:], " ")
	}

	sc, err := d.ss.Start()
	if err != nil {
		return "", fmt.Errorf("cmd_leave: Start failed: %w", err)
	}

	res, err := d.tools.Leave(sc.World, to, skip, sc.Character)
	if err != nil {
		return "", fmt.Errorf("wrap: %w", err)
	}

	d.commit(ctx, fmt.Sprintf("world leave: %s -> %s", sc.World, to))

	out := fmt.Sprintf("Мир %s (день %d) покинут.\nАктивный мир: %s", res.FromWorld, res.FromDay, res.NewWorld)
	if res.NewWorldInit {
		out += " (инициализирован)"
	}

	return out, nil
}

func (d *Dispatcher) cmdReturn(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	if len(msg.Args) < 2 {
		return "Использование: /return <мир> <дней_прошло>", nil
	}

	world := msg.Args[0]
	days := msg.Args[1]

	note, err := d.tools.ReturnWorld(world, days)
	if err != nil {
		return "", fmt.Errorf("cmd_return: ReturnWorld failed: %w", err)
	}

	d.commit(ctx, "world return: "+world)

	return note, nil
}

// cmdReload forces the active world's data source to refresh
// from the canonical source. Today the source is "files",
// so a successful Reload is a no-op (every read already goes
// to disk); the command still does two useful things:
//
//  1. Surfaces a one-line "backed by: files" reply so the
//     player sees the wiring is alive.
//  2. Future backends (cached, s3, git-lfs) override Reload
//     to invalidate their LRU. The command becomes a
//     forced-refresh knob for the operator.
//
// Typical use: the operator edits canon.md or state.md by
// hand to fix a typo, then runs /reload to confirm the
// toolset is ready for the next turn.
func (d *Dispatcher) cmdReload() (string, error) {
	if d.tools == nil {
		return "toolset не подключён.", nil
	}

	if r, ok := d.tools.(usecase.Reloadable); ok {
		if err := r.Reload(); err != nil {
			//nolint:nilerr // UX: surface reload failure as a user-facing warning, not a transport error
			return "⚠️ reload: " + err.Error(), nil
		}
	}
	// /reload forces the operator's hand-edited state.md /
	// lore.md to be picked up. We drop the cached
	// worldStateSnapshot so the next turn rebuilds index:1
	// from disk, AND we walk every per-chat conversation
	// and clear it — the player re-starts from a clean
	// dialogue while the LLM still sees the fresh world
	// state. This is more aggressive than compaction
	// (which keeps the last 2-3 turns) because /reload
	// means "I edited the world, throw away the in-memory
	// chat history".
	if d.gm != nil {
		d.gm.InvalidateWorldState("reload")
		d.gm.ResetAllConversations()
	}

	if d.slow != nil {
		_ = d.slow.Write("tool.reload", "", map[string]any{"source": "files"})
	}

	return "✅ reload ok. backed by: files. Следующий ход подхватит свежие данные, чат сброшен.", nil
}

func (d *Dispatcher) cmdHelp() (string, error) {
	cmds := []string{
		"/start — загрузить info.yaml и world state",
		"/launch <перс> <мир> [канон] — первоначальная настройка",
		"/status — текущий персонаж/мир/world state",
		"/me — содержимое SOUL/SKILL/memory/state персонажа",
		"/endday <N> <выжимка> — записать день в chronicle",
		"/maintenance — выжимка NPC > 40 строк",
		"/leave <мир> [время] — переход в новый мир",
		"/return <мир> <дней> — возврат с time-skip",
		"/reload — перечитать данные активного мира из источника",
		"/save — коммит + push, уведомление отдельным сообщением",
		"/commit <msg> — коммит (только локально)",
		"/push — pull --rebase + push",
	}
	sort.Strings(cmds)

	return "Команды:\n" + strings.Join(cmds, "\n"), nil
}
