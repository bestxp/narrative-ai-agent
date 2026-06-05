# Lazy Universe — Telegram-бот для нарративной RPG

Go-бот, реализующий скилл **lazy-universe** (Lazy Multiverse):
Game Master ведёт историю игрока в нескольких мирах, между
которыми тот путешествует. Состояние (`info.yaml`, `state.md`,
`memorise.md`, `lore.md`, `plan.md`, профили NPC) хранится
**plain files** под git; LLM (Ollama / OpenRouter / vLLM /
OpenAI-compatible) генерирует ответы, обновляет файлы и пишет в
шпаргалку на следующий ход.

## Архитектура

```
internal/
  config/        YAML-конфиг, валидация
  logging/       zerolog обёртка
  slowlog/       покадровый аудит в JSON-lines файл
  domain/        чистые структуры (Info, Tool, PromptContext)
  adapter/
    storage/     FileStore — единственное место, где бот трогает диск
    gitops/      git add/commit/push с rebase-fallback
    llm/         OpenAI-compatible HTTP+SSE клиент
  messaging/
    telegram/    tg Bot API, streaming с throttling
  usecase/
    FirstLaunch / SessionStart / Maintenance / WorldTransition
    NPCManager / CharacterUpdate / ResponseFormat / GM
  dispatcher/    routing команд + streaming entry point
cmd/bot/main.go  wiring
```

- **Clean Architecture**: usecase не знают про telegram или LLM
  клиент, только интерфейсы.
- **TDD**: 168 тестов, `go test -count=1 ./...` зелёный.
- **Multi-transport**: добавление Discord = новая реализация
  `messaging.Client` + поле в `MessagingConfig`. Бот-логика не
  меняется.
- **Multi-role LLM**: одна роль сейчас (`narrative`); добавление
  `summary` для сжатия NPC = ключ в `llm.roles`.

## Запуск

```bash
# 1. заполнить config.yaml (токен бота, allowed_user_ids, llm.roles.narrative)
# 2. запустить
./bin/bot-linux-amd64           # или .exe / darwin*
./bin/bot-linux-amd64 --log-level=debug --no-llm  # dry run
```

Build-матрица: `make build-all` → `bin/bot-{linux,darwin,windows}-{amd64,arm64}`.

## Конфигурация (config.yaml)

| Секция | Ключ | Default | Что делает |
|---|---|---|---|
| `messaging.telegram` | `token` | — | BotFather токен |
| | `polling_timeout` | 60 | секунды long-poll |
| | `parse_mode` | `""` | plain по умолчанию (Markdown ломает Telegram API) |
| | `allowed_user_ids` | — | whitelist |
| `paths` | `data_root` | `game-data` | корень `info.yaml`/characters/worlds |
| | `git_workdir` | `.` | где запускать git |
| `git` | `remote` / `branch` | `origin` / `master` | sync target |
| | `commit_author` / `commit_email` | `Lazy Universe Bot` | local-only git identity |
| | `remote_disabled` | `false` | `true` = только локальные коммиты |
| | `auto_save.after_messages` | `5` | auto-commit + push каждые N ответов |
| | `verbose_save` | `false` | расширенный формат "✅ сохранено" |
| `narrative` | `word_limit` | `350` | мягкий лимит на ответ GM |
| | `language` | `ru` | |
| | `rules_check_block` | `false` | **см. ниже** |
| `llm` | `token_tracking` | `off` | `off` / `estimate` / `usage` |
| | `include_in_reply` | `true` | дописывать ли "🔢 ~N tok" в конец |
| | `default_timeout_seconds` | `120` | HTTP timeout для ролей без своего |
| | `roles.narrative` | — | обязательная роль |
| `slowlog` | `enabled` | `false` | `true` = писать в `slow.log` |
| | `file` | `slow.log` | JSON-lines per-request audit |

## `narrative.rules_check_block` — что это и почему так

LLM по промпту `prompts/narrative.md` пишет ответ в три блока:

```
**диалоги и действия**
<нарратив>

**КОНТЕКСТ И ИЗМЕНЕНИЯ**
<что изменилось: state, NPC, файлы>

**ВАЛИДАЦИЯ ПРАВИЛ**
- Лимит слов: 171 / 350
- Управлял персонажем игрока: нет
- NPC знал только то, что должен: да
- Файлы обновлены: NPC anbu_dog
```

Первые два — нарратив, нужны игроку. Третий — это **self-report LLM**
(«я не нарушил правила»). В production он шумит: перегружает
экран, повторяет то, что и так понятно из нарратива, и при
многоходовых диалогах становится основным «весом» сообщения.

**Решение**: флаг `narrative.rules_check_block` (default `false`).

| `rules_check_block` | Что видит игрок | Что пишет LLM |
|---|---|---|
| `false` (default) | только `**диалоги**` + `**КОНТЕКСТ**` | всё (стрипается на выходе) |
| `true` (debug) | все три блока | всё |

### Почему не убираем блок из system prompt

Альтернатива — сказать LLM «не пиши `**ВАЛИДАЦИЯ ПРАВИЛ**`». Это
**ухудшает** качество ответов:

1. **Self-report работает как обратная связь**. LLM, видя в
   инструкции «я должен перечислить, что проверил», реже
   нарушает правила. Без этого пункта `Управлял персонажем
   игрока: нет` начнут проскакивать «ты усмехнулся» и подобное.
2. **Контекст тратится на размышления о форме**. Без явного
   требования self-report LLM пишет чуть короче и чаще
   забывает структуру.
3. **Модель < 14B** (qwen2.5:7b, llama3.1:8b) без жёсткого
   формата ответа выдаёт «простыню» вместо трёх блоков.

Эмпирически это основная причина, по которой мы **не**
трогаем system prompt: на 7B моделях (Ollama локально)
compliance с правилами заметно падает, и эффект перевешивает
экономию ~100-180 output-токенов на ход (~2-5% от размера
round'а). Стоимость сэкономленных токенов на hosted API
незначительна (порядка цента за сессию), а деградация качества
неприятна.

Поэтому **стрипаем на выходе**, а не убираем из промпта. LLM
всё ещё «думает», что отчитывается; игрок этого не видит.

Альтернативы вроде «компактного self-report в одну строку» или
«полного удаления из промпта» рассмотрены и **не реализуются**:
любая экономия output-токенов ценой compliance regression — плохая
сделка на текущем размере моделей. Если в будущем перейдём на
14B+ или 70B+ и compliance станет устойчивым, можно вернуться к
этому вопросу.

### Где применяется стрип

- `internal/domain/rules_block.go` — `StripRulesBlock(text)`.
  Regex-якорь по началу строки, поэтому «ВАЛИДАЦИЯ ПРАВИЛ» внутри
  цитаты NPC не задевается.
- `cmd/bot/main.go` — `handleIncoming` вызывает стрип в `OnDelta`
  (на каждом chunk'е) и в финальном `replyBuf` перед `Final` /
  `Send`. Идемпотентно — повторный вызов безвреден.
- `--no-llm` режим: блок рисует dispatcher, **не стрипается** —
  это единственный способ увидеть валидацию без LLM, и в dry-run
  он полезен.

## Slowlog — покадровый аудит

При `slowlog.enabled: true` бот пишет JSON-lines в `slow.log`:

```json
{"time":"2026-06-05T22:30:00Z","kind":"incoming.text","chat":"167898078","fields":{"text":"конец дня"}}
{"time":"2026-06-05T22:30:01Z","kind":"llm.tokens","chat":"167898078","fields":{"round":0,"prompt_tokens":2476,"completion_tokens":178,"total_tokens":2654,"source":"usage"}}
{"time":"2026-06-05T22:30:02Z","kind":"character.update","chat":"167898078","fields":{"character":"markus","file":"SOUL.md","section":"Имя","appended":"Маркус, 13 лет"}}
```

Виды событий:

| Kind | Когда |
|---|---|
| `incoming.text` | входящее сообщение от игрока |
| `llm.tokens` | каждый round LLM (prompt+completion+total+source) |
| `llm.request` | полный дамп запроса (в планах, см. ниже) |
| `character.update` | `update_character` tool |
| `tool.*` | прочие tools (state/plan/NPC) |
| `auto-save` | `git commit` от auto-save |
| `git.push` | `git push` |

Файл растёт постепенно, ротация вручную (`mv slow.log slow.log.1`).

## Token tracking

| `llm.token_tracking` | Источник числа | Точность |
|---|---|---|
| `off` | — | не считает |
| `estimate` | `len(text) / 4` | грубо, но работает на любом API (Ollama) |
| `usage` | `usage` block от провайдера | точно; fallback в estimate если провайдер не вернул (Ollama) |

Slowlog всегда пишет фактические числа, в `include_in_reply: true`
дописывает `🔢 ~1234 tok (usage)` в конец ответа.

## Команды Telegram

| Команда | Что делает |
|---|---|
| `/launch <имя> <мир> [канон]` | первоначальная настройка (SOUL/SKILL/memory + world dirs) |
| `/start` | snapshot `info.yaml` + `state.md` |
| `/status` | короткий статус (персонаж, мир, state.md) |
| `/me` | SOUL/SKILL/memory/state персонажа (обрезано до ~40 строк на секцию) |
| `/endday <N> <выжимка>` | записать день в `memorise.md` |
| `/maintenance` | сжать NPC > 40 строк |
| `/leave <мир> [время]` | переход в новый мир |
| `/return <мир> <дней>` | возврат с time-skip |
| `/save` | `git commit` + `git push` (если не remote_disabled), уведомление |
| `/commit <msg>` | коммит только |
| `/push` | pull-rebase-push |
| `/help` | список команд |

**Auto-save**: каждые `git.auto_save.after_messages` ответов бота
(freeform только, не команды) бот автоматически коммитит+пушит
и **шлёт отдельное сообщение** «✅ сохранено: commit abc1234».

## Стриминг

`StartStream` отправляет placeholder `…`, далее `OnStatus`/`OnDelta`
вращают плейсхолдер через фазы (`…собираю контекст`,
`…спрашиваю qwen2.5`, `…применяю end_day`) пока не придёт первый
текстовый delta — дальше текст выигрывает. `Final` отправляет
итоговый текст (с токен-строкой если включено). Throttle на
editMessageText — 700 ms между вызовами.

`ThrottledStream` оборачивает `*stream` (см.
`internal/messaging/telegram/stream.go`); Final-метод
**idempotentен** — повторный вызов no-op.

## Tool reference (LLM)

| Tool | Файл | Эффект |
|---|---|---|
| `end_day` | `domain/tools.go` | `memorise.md` + `state.md` |
| `update_state` | | `state.md` (момент + NPC + in_flight) |
| `run_maintenance` | | сжатие NPC |
| `rotate_plan` | | `plan.md` (3-5 событий) |
| `create_npc` | | новый NPC + registry |
| `leave_world` | | переключение мира |
| `update_character` | | SOUL/SKILL/memory персонажа |

`update_character` добавлен специально: при `/leave` все
`state.md` остаются в прошлом мире, и если новая инфа про
персонажа лежала только там — теряется. Tool пишет в
`characters/<name>/SOUL.md|SKILL.md|memory.md` секции, не
перезаписывая существующее.

## Известные ограничения

- **Truncation при больших диалогах**: `max_tokens: 1500` →
  при 4k+ токенов промпта LLM может упереться в cap и обрезать
  ответ. Решения: `run_maintenance` (сжатие), повысить
  `max_tokens` в `llm.roles.narrative`.
- **История диалога теряется при рестарте бота** (живёт в RAM
  как `sync.Map`). `state.md` помнит нарратив, но не дословные
  реплики. Смягчение: бот начинает «новый разговор» с тем же
  персонажем в том же мире.
- **Ollama не отдаёт `usage` block** — token tracking всегда
  fallback в `estimate` при использовании Ollama.
- **MarkdownV2/HTML parse_mode** в Telegram ломает на
  спецсимволах (`.`, `!`, `-`). По умолчанию `parse_mode: ""`
  (plain text).

## Roadmap

- [ ] `llm.request`/`llm.response` полный дамп в slowlog (для
  дебага prompt construction)
- [ ] Auto-save counter persist на диск (переживает рестарт)
- [ ] `/continue` tool при finish_reason="length" (truncation
  recovery)
- [ ] Persist LLM-история в `last_session.md` (compress
  прерывание)
- [ ] `summary` LLM-роль для NPC compaction вместо regex-стрипа
- [ ] Discord transport

## Лицензия

Внутренний PoC.
