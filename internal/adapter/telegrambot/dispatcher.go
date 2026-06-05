package telegrambot

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"narrative/internal/adapter/gitops"
	"narrative/internal/adapter/storage"
	"narrative/internal/config"
	"narrative/internal/usecase"
)

// Dispatcher is the production Handler: it routes Telegram commands
// to usecases and composes the response.
type Dispatcher struct {
	cfg   *config.Config
	fs    *storage.FileStore
	git   *gitops.Operator
	rf    *usecase.ResponseFormat
	fl    *usecase.FirstLaunch
	ss    *usecase.SessionStart
	npcm  *usecase.NPCManager
	mt    *usecase.Maintenance
	wt    *usecase.WorldTransition
	log   zerolog.Logger
}

func NewDispatcher(cfg *config.Config, fs *storage.FileStore, git *gitops.Operator) *Dispatcher {
	return NewDispatcherWithLogger(cfg, fs, git, zerolog.Nop())
}

func NewDispatcherWithLogger(cfg *config.Config, fs *storage.FileStore, git *gitops.Operator, log zerolog.Logger) *Dispatcher {
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

func (d *Dispatcher) Handle(ctx *Context) (string, error) {
	if ctx.Command == "" {
		return d.handleFreeform(ctx)
	}
	d.log.Info().Int("user_id", ctx.UserID).Str("command", ctx.Command).Strs("args", ctx.Args).Msg("dispatch")
	switch ctx.Command {
	case "start":
		return d.cmdStart()
	case "launch":
		return d.cmdLaunch(ctx)
	case "status":
		return d.cmdStatus()
	case "commit":
		return d.cmdCommit(ctx)
	case "push":
		return d.cmdPush()
	case "maintenance":
		return d.cmdMaintenance()
	case "endday":
		return d.cmdEndDay(ctx)
	case "leave":
		return d.cmdLeave(ctx)
	case "return":
		return d.cmdReturn(ctx)
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

func (d *Dispatcher) handleFreeform(ctx *Context) (string, error) {
	v := d.rf.Validate(ctx.RawText)
	body := fmt.Sprintf("**диалоги и действия**\n%s\n\n**КОНТЕКСТ И ИЗМЕНЕНИЯ**\nбез изменений\n\n**ВАЛИДАЦИЯ ПРАВИЛ**\n- Лимит слов: %d\n- Слов в ответе: %d\n- Превышен: %v\n- Запрещённые формы: %v\n",
		ctx.RawText, d.cfg.Narrative.WordLimit, v.WordCount, v.OverLimit, v.ForbiddenForms)
	return body, nil
}

func (d *Dispatcher) cmdStart() (string, error) {
	if !d.fs.Exists("info.md") {
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

func (d *Dispatcher) cmdLaunch(ctx *Context) (string, error) {
	if len(ctx.Args) < 2 {
		return "Использование: /launch <имя_перса> <имя_мира> [канон/сценарий...]", nil
	}
	charName := ctx.Args[0]
	worldName := ctx.Args[1]
	canon := strings.Join(ctx.Args[2:], " ")
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
	if !d.fs.Exists("info.md") {
		return "Нет активной сессии. /launch сначала.", nil
	}
	sc, err := d.ss.Start()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Персонаж: %s\nМир: %s\n\n**state.md**\n%s", sc.Character, sc.World, sc.State), nil
}

func (d *Dispatcher) cmdCommit(ctx *Context) (string, error) {
	if d.git == nil {
		return "git не инициализирован (тестовый режим).", nil
	}
	msg := "auto: maintenance"
	if len(ctx.Args) > 0 {
		msg = strings.Join(ctx.Args, " ")
	}
	if err := d.git.CommitAll(msg); err != nil {
		return "", err
	}
	return "git commit выполнен.", nil
}

func (d *Dispatcher) cmdPush() (string, error) {
	if d.git == nil {
		return "git не инициализирован.", nil
	}
	if err := d.git.SyncRebase(); err != nil {
		return "", err
	}
	return "git push выполнен.", nil
}

func (d *Dispatcher) cmdMaintenance() (string, error) {
	if !d.fs.Exists("info.md") {
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

func (d *Dispatcher) cmdEndDay(ctx *Context) (string, error) {
	if len(ctx.Args) < 2 {
		return "Использование: /endday <номер_дня> <краткая_выжимка...>", nil
	}
	day, err := strconv.Atoi(ctx.Args[0])
	if err != nil {
		return "", fmt.Errorf("day must be int: %w", err)
	}
	summary := strings.Join(ctx.Args[1:], " ")
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

func (d *Dispatcher) cmdLeave(ctx *Context) (string, error) {
	if len(ctx.Args) < 1 {
		return "Использование: /leave <новый_мир> [прошло_времени]", nil
	}
	to := ctx.Args[0]
	skip := ""
	if len(ctx.Args) > 1 {
		skip = strings.Join(ctx.Args[1:], " ")
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
	msg := fmt.Sprintf("Мир %s (день %d) покинут.\nАктивный мир: %s", res.FromWorld, res.FromDay, res.NewWorld)
	if res.NewWorldInit {
		msg += " (инициализирован)"
	}
	return msg, nil
}

func (d *Dispatcher) cmdReturn(ctx *Context) (string, error) {
	if len(ctx.Args) < 2 {
		return "Использование: /return <мир> <дней_прошло>", nil
	}
	world := ctx.Args[0]
	days := ctx.Args[1]
	note, err := d.wt.ReturnWorld(world, days)
	if err != nil {
		return "", err
	}
	d.commit(fmt.Sprintf("world return: %s", world))
	return note, nil
}

func (d *Dispatcher) cmdHelp() (string, error) {
	cmds := []string{
		"/start — загрузить info.md и state.md",
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
