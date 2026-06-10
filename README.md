# Lazy Universe — NarRPG Bot

Game Master бот для нарративной RPG. Ведёт историю игрока, управляет мирами, NPC и состоянием через plain files + git. Работает через Telegram и VK.

## Запуск

```bash
# Настроить config.yaml (токены, LLM, allowed_user_ids)
go build -o bin/bot ./cmd/bot
./bin/bot --config running/config.yaml

# Dry-run без LLM (echo + валидация)
./bin/bot --config running/config.yaml --no-llm --log-level=debug
```

Cross-compile: `make build-all` → `bin/bot-{linux,darwin,windows}-{amd64,arm64}`.

## Транспорты

Бот поддерживает одновременную работу нескольких мессенджеров:

- **Telegram** — long-poll Bot API, streaming (editMessageText с throttling 700ms)
- **VK** — Bots Long Poll API (v3), streaming (messages.edit)

Хотя бы один транспорт должен быть настроен. VK пропускается, если `access_token` или `group_id` пустые.

### VK: требуемые разрешения токена

Manage → Settings → API usage → Create token, включить:

- **messages** — отправка, редактирование, чтение сообщений
- **messages.setActivity** — индикатор «печатает…»
- **groups** — Long Poll (getLongPollServer, setLongPollSettings)

## Команды

| Команда | Что делает |
|---|---|
| `/launch <имя> <мир> [канон]` | первоначальная настройка персонажа и мира |
| `/start` | snapshot info.yaml + state.md |
| `/status` | короткий статус (персонаж, мир, state.md) |
| `/me` | SOUL/SKILL/memory/state персонажа |
| `/endday <N> <выжимка>` | записать день в memorise.md |
| `/maintenance` | сжать NPC > 40 строк |
| `/leave <мир> [время]` | переход в новый мир |
| `/return <мир> <дней>` | возврат с time-skip |
| `/save` | git commit + push |
| `/commit <msg>` | коммит только |
| `/push` | pull-rebase-push |
| `/help` | список команд |

Auto-save: каждые `git.auto_save.after_messages` ответов бот коммитит и пушит.

## Конфигурация

См. `config.yaml` — все параметры с комментариями. Ключевые:

| Секция | Параметр | Default | Описание |
|---|---|---|---|
| `messaging.telegram` | `token` | — | BotFather токен |
| | `parse_mode` | `"HTML"` | режим парсинга (plain/MarkdownV2/HTML) |
| | `allowed_user_ids` | — | whitelist user_id |
| | `reply_to_user` | `true` | thread-ответы на сообщения игрока |
| `messaging.vk` | `access_token` | — | VK community token |
| | `group_id` | — | ID группы VK |
| | `allowed_user_ids` | — | whitelist user_id |
| `narrative` | `word_limit` | `250` | мягкий лимит слов на ответ |
| | `rules_check_block` | `false` | показывать ли блок ВАЛИДАЦИЯ ПРАВИЛ |
| `git` | `disabled` | `false` | полностью отключить git (коммиты, push, auto-save) |
| | `remote_disabled` | `false` | локальные коммиты без push |
| `llm` | `driver` | `"openai"` | `openai` или `anthropic` |
| | `token_tracking` | `"estimate"` | `off` / `estimate` / `usage` |
| | `roles.narrative` | — | обязательная роль (модель, URL, промпт) |
| | `roles.summary` | — | роль сжатия (опционально) |
| `slowlog` | `enabled` | `false` | JSON-lines аудит в slow.log |

## rules_check_block

LLM пишет три блока: **диалоги**, **КОНТЕКСТ**, **ВАЛИДАЦИЯ ПРАВИЛ**. Третий — self-report модели о соблюдении правил. В production (`false`) блок стрипается на выходе, игрок видит только нарратив и контекст. LLM по-прежнему «думает», что отчитывается — compliance не падает.

## Slowlog

`slowlog.enabled: true` → JSON-lines в `slow.log`: LLM запросы/ответы, токены, tool calls, входящие/исходящие сообщения. Ротация вручную (`mv slow.log slow.log.1`).

## LLM драйверы

- **openai** (default) — /v1/chat/completions (Ollama, OpenAI, OpenRouter, routerai.ru)
- **anthropic** — /v1/messages (Anthropic direct, Ollama Cloud /v1/messages, OpenRouter)

Оба драйвера: tool_choice=auto, 8 prod schemas, streaming SSE. Anthropic драйвер поддерживает prompt caching (system + первый user message + последний tool).

## Известные ограничения

- История диалога живёт в RAM, при рестарте бота теряется. state.md сохраняет нарратив, но не дословные реплики.
- Ollama не отдаёт usage block — token tracking fallback в `estimate`.
- MarkdownV2/HTML parse_mode в Telegram может ломаться на спецсимволах. Default: `"HTML"` с автоматической конвертацией Markdown→HTML.