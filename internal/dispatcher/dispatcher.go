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

	"github.com/rs/zerolog"

	"narrative/internal/adapter/gitops"
	"narrative/internal/adapter/storage"
	"narrative/internal/config"
	"narrative/internal/messaging"
	"narrative/internal/usecase"
)

// Dispatcher turns an IncomingMessage into a reply string. It is the
// single point that knows about /commands; everything else flows
// through the GM.
type Dispatcher struct {
	cfg  *config.Config
	fs   *storage.FileStore
	git  *gitops.Operator
	rf   *usecase.ResponseFormat
	fl   *usecase.FirstLaunch
	ss   *usecase.SessionStart
	npcm *usecase.NPCManager
	mt   *usecase.Maintenance
	wt   *usecase.WorldTransition
	gm   *usecase.GM // optional; nil until main.go wires the LLM
	log  zerolog.Logger
}

func New(cfg *config.Config, fs *storage.FileStore, git *gitops.Operator, log zerolog.Logger) *Dispatcher {
	l := log.With().Str("component", "dispatcher").Logger()
	return &Dispatcher{
		cfg:  cfg,
		fs:   fs,
		git:  git,
		rf:   usecase.NewResponseFormat(cfg.Narrative.WordLimit, cfg.Narrative.Language),
		fl:   usecase.NewFirstLaunchWithLogger(fs, l),
		ss:   usecase.NewSessionStartWithLogger(fs, l),
		npcm: usecase.NewNPCManagerWithLogger(fs, l),
		mt:   usecase.NewMaintenanceWithLogger(fs, l),
		wt:   usecase.NewWorldTransitionWithLogger(fs, l),
		log:  l,
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
func (d *Dispatcher) Handle(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	d.log.Info().Str("chat", msg.ChatID).Str("command", msg.Command).Strs("args", msg.Args).Msg("dispatch")
	if msg.Command == "" {
		return d.handleFreeform(ctx, msg)
	}
	switch msg.Command {
	case "start":
		return d.cmdStart()
	case "launch":
		return d.cmdLaunch(msg)
	case "status":
		return d.cmdStatus()
	case "commit":
		return d.cmdCommit(msg)
	case "push":
		return d.cmdPush()
	case "maintenance":
		return d.cmdMaintenance()
	case "endday":
		return d.cmdEndDay(msg)
	case "leave":
		return d.cmdLeave(msg)
	case "return":
		return d.cmdReturn(msg)
	case "help":
		return d.cmdHelp()
	}
	return "", nil
}

func (d *Dispatcher) commit(msg string) {
	if d.git == nil {
		return
	}
	_ = d.git.CommitAll(msg)
}

func (d *Dispatcher) handleFreeform(ctx context.Context, msg messaging.IncomingMessage) (string, error) {
	// If a GM is wired, stream the LLM's response back. Otherwise fall
	// back to the legacy echo + validation behaviour.
	if d.gm == nil {
		v := d.rf.Validate(msg.Text)
		return fmt.Sprintf("**диалоги и действия**\n%s\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- Лимит слов: %d\n- Слов в ответе: %d\n- Превышен: %v\n- Запрещённые формы: %v\n",
			msg.Text, d.cfg.Narrative.WordLimit, v.WordCount, v.OverLimit, v.ForbiddenForms), nil
	}
	var buf strings.Builder
	if err := d.gm.Reply(ctx, msg.ChatID, msg.Text, func(delta string) error {
		buf.WriteString(delta)
		return nil
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (d *Dispatcher) cmdStart() (string, error) {
	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активной сессии. Используйте /launch <персонаж> <мир> для первоначальной настройки.", nil
	}
	sc, err := d.ss.Start()
	if err != nil {
		return "", err
	}
	warn := ""
	if len(sc.Warnings) > 0 {
		warn = "\nПредупреждения:\n- " + strings.Join(sc.Warnings, "\n- ")
	}
	if sc.SyncMemoriseAhead {
		warn += "\n⚠️ memorise.md опережает state.md — галлюцинация? Спросить игрока."
	}
	if sc.SyncStateAhead {
		warn += "\n⚠️ state.md опережает memorise.md — дописать недостающие дни в memorise.md."
	}
	return fmt.Sprintf("**Сессия запущена**\nПерсонаж: %s\nМир: %s\n\n**state.md**\n%s%s",
		sc.Character, sc.World, sc.State, warn), nil
}

func (d *Dispatcher) cmdLaunch(msg messaging.IncomingMessage) (string, error) {
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
		return "", err
	}
	d.commit("first launch: " + charName + " / " + worldName)
	return fmt.Sprintf("**Создано**\nПерсонаж: %s\nМир: %s\nИспользуйте /start для просмотра state.md.", charName, worldName), nil
}

func (d *Dispatcher) cmdStatus() (string, error) {
	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активной сессии. /launch сначала.", nil
	}
	sc, err := d.ss.Start()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Персонаж: %s\nМир: %s\n\n**state.md**\n%s", sc.Character, sc.World, sc.State), nil
}

func (d *Dispatcher) cmdCommit(msg messaging.IncomingMessage) (string, error) {
	if d.git == nil {
		return "git не инициализирован (тестовый режим).", nil
	}
	commitMsg := "auto: maintenance"
	if len(msg.Args) > 0 {
		commitMsg = strings.Join(msg.Args, " ")
	}
	if err := d.git.CommitAll(commitMsg); err != nil {
		return "", err
	}
	return "git commit выполнен.", nil
}

func (d *Dispatcher) cmdPush() (string, error) {
	if d.git == nil {
		return "git не инициализирован.", nil
	}
	if d.git.RemoteDisabled() {
		return "git push пропущен: remote_disabled=true (только локальные коммиты).", nil
	}
	if err := d.git.SyncRebase(); err != nil {
		return "", err
	}
	return "git push выполнен.", nil
}

func (d *Dispatcher) cmdMaintenance() (string, error) {
	if !d.fs.Exists(storage.InfoFile) {
		return "Нет активного мира.", nil
	}
	sc, err := d.ss.Start()
	if err != nil {
		return "", err
	}
	touched, err := d.mt.CompactNPCs(sc.World)
	if err != nil {
		return "", err
	}
	d.commit(fmt.Sprintf("maintenance: %s", sc.World))
	if len(touched) > 0 {
		return "Обслуживание выполнено. Выжимка NPC: " + strings.Join(touched, ", "), nil
	}
	return "Обслуживание выполнено. Выжимка NPC: не требуется.", nil
}

func (d *Dispatcher) cmdEndDay(msg messaging.IncomingMessage) (string, error) {
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
		return "", err
	}
	if err := d.mt.ArchiveDay(sc.World, day, summary); err != nil {
		return "", err
	}
	d.commit(fmt.Sprintf("День %d", day))
	return fmt.Sprintf("День %d заархивирован в memorise.md.", day), nil
}

func (d *Dispatcher) cmdLeave(msg messaging.IncomingMessage) (string, error) {
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
		return "", err
	}
	res, err := d.wt.Leave(sc.World, to, skip, sc.Character)
	if err != nil {
		return "", err
	}
	d.commit(fmt.Sprintf("world leave: %s -> %s", sc.World, to))
	out := fmt.Sprintf("Мир %s (день %d) покинут.\nАктивный мир: %s", res.FromWorld, res.FromDay, res.NewWorld)
	if res.NewWorldInit {
		out += " (инициализирован)"
	}
	return out, nil
}

func (d *Dispatcher) cmdReturn(msg messaging.IncomingMessage) (string, error) {
	if len(msg.Args) < 2 {
		return "Использование: /return <мир> <дней_прошло>", nil
	}
	world := msg.Args[0]
	days := msg.Args[1]
	note, err := d.wt.ReturnWorld(world, days)
	if err != nil {
		return "", err
	}
	d.commit(fmt.Sprintf("world return: %s", world))
	return note, nil
}

func (d *Dispatcher) cmdHelp() (string, error) {
	cmds := []string{
		"/start — загрузить info.yaml и state.md",
		"/launch <перс> <мир> [канон] — первоначальная настройка",
		"/status — текущий персонаж/мир/state.md",
		"/endday <N> <выжимка> — записать день в memorise.md",
		"/maintenance — выжимка NPC > 40 строк",
		"/leave <мир> [время] — переход в новый мир",
		"/return <мир> <дней> — возврат с time-skip",
		"/commit <msg> / /push — git",
	}
	sort.Strings(cmds)
	return "Команды:\n" + strings.Join(cmds, "\n"), nil
}
