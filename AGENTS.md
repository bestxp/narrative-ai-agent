# AGENTS.md — проектные правила для агентных редакторов

## Правило: LLM работает с доменами, а не с путями хранения

**Контракт.** Любой текст, который увидит LLM (system
prompt, tool description, parameter description, header
в user-сообщении, system-state snapshot, slowlog-блок
который LLM читает, ошибка которая возвращается в
ToolResult) — должен называть **доменную сущность**, а
не storage-путь.

### Что считается доменом, а что — storage-уровнем

**Домен** (✅ для LLM):
- soul / skill / memory / inventory
- world state / plan / chronicle / lore / canon
- personal memory / last update / relations

**Storage** (❌ для LLM):
- `*.yaml`, `*.md`, `*.json` (расширения файлов)
- `characters/<dir>/...` (пути в файловом дереве)
- `state.md` / `memorise.md` / `lore.md` / `canon.md` /
  `plan.md` / `memory.yaml` / `skill.yaml` / `SOUL.yaml` /
  `inventory.yaml` (имена файлов)
- `ReadRaw("...")`, `WriteRawAtomic("...")` (file paths
  в коде — это вообще не LLM-facing, но паттерн «вижу
  строку с путём в LLM-facing коде» — это уже баг)

### Где следить

Перед коммитом пройти grep по этим местам:

- `internal/prompts/*.md` — system prompts
- `internal/domain/prompt.go` — сборка system / user message
- `internal/domain/tools.go` — tool descriptions (Description,
  StringEnum description, parameter description)
- `internal/domain/system_state.go` — если рендерится в prompt
- `internal/structured/*.go` — JSON schema для LLM
- Любой `Description:` / `String("...")` / `StringEnum("...", ...)`
  в коде, который идёт в LLM-facing JSON

Grep-команды (использовать при code review / перед merge):

```bash
# 1. Расширения файлов в LLM-facing коде
grep -nE '\.(yaml|yml|md|json|txt)\b' \
    internal/prompts/*.md \
    internal/domain/prompt.go \
    internal/domain/tools.go \
    internal/structured/*.go

# 2. File paths в LLM-facing коде
grep -nE 'characters/.*\.(yaml|md)|worlds/.*\.(yaml|md)' \
    internal/prompts/*.md \
    internal/domain/prompt.go \
    internal/domain/tools.go
```

### Правила замены (если нашёл)

| ❌ Не пиши в LLM | ✅ Пиши |
|---|---|
| `memory.yaml` | memory |
| `skill.yaml` | skill |
| `SOUL.yaml` | soul |
| `inventory.yaml` | inventory |
| `state.md` | world state |
| `plan.md` | plan |
| `memorise.md` | chronicle |
| `lore.md` | lore |
| `canon.md` | canon |
| `characters/<active>/X.yaml` | X активного персонажа |
| `worlds/<active>/state.md` | world state активного мира |

### Где правило НЕ действует

- **Код-комментарии** (`//` в `.go`, docstring'и функций) — это
  для разработчика, не для LLM. Здесь `state.md`,
  `memorise.md`, `memory.yaml` — допустимы и желательны
  (это единственный способ объяснить что где лежит).
- **Storage paths в runtime-коде** (`"characters/" + dir +
  "/memory.yaml"`) — это file paths, не LLM-facing.
- **Имена файлов промтов** в `//go:embed *.md` и
  `LoadSystemPrompt(name)` — это путь к embedded ресурсу
  бинаря, не текст для LLM.
- **Тесты** проверяющие storage paths (`assert.Contains(body,
  "narrative.md")`) — это test fixtures, не LLM-facing.

### Почему это важно

Storage-уровень — это **деталь реализации**. LLM не должна
знать, что soul хранится в YAML под именем персонажа в
`characters/<dir>/`. Если завтра мы переедем на MongoDB /
SQLite / git-backed blob store, LLM-контракт не должен
измениться. Меняется storage — меняется адаптер, не
промты и не tool descriptions.

Расширения в LLM-facing коде = **протечка абстракции**:
сегодня `.yaml`, завтра `.toml`, послезавтра `.bson` —
но если LLM видела `memory.yaml` в tool description, она
начнёт думать, что это часть контракта. Это не контракт.
Это деталь хранения.

### При нарушении

1. **Code review** — отклонить PR с LLM-facing упоминанием
   storage-уровня.
2. **Тест-фикстуры** — если тест валидирует storage-path
   в LLM-facing тексте (`assert.Contains(body, "memorise.md")`),
   это значит: (а) либо storage path действительно нужен в
   LLM (тогда правило не действует — задокументируй почему),
   (б) либо тест устарел и проверяет старую терминологию —
   обновить ассерт.
3. **Миграция промтов** — если в существующем промте есть
   `memorise.md`, при следующем редактировании заменить на
   `chronicle` (или эквивалент домена).

### Связанные правила

- **Не пиши в коде секреты** (API keys, tokens, VK tokens)
  — должны быть в `config.yaml` и в `.gitignore`, не в
  исходниках. `git ls-files` + grep по `vk1\.a\.` /
  `sk-[a-zA-Z0-9]{20,}` / `AIza[0-9A-Za-z_-]{20,}` перед
  коммитом.
- **Не пиши `name` в skill.yaml / memory.yaml /
  inventory.yaml** — display name только в SOUL.yaml.
  Идентичность через `characters/<dir>/` путь, не через
  поле.
- **Strict enum при Append/Replace, non-strict при
  MigrateFromMarkdown** для memory — старые секции не
  должны теряться при миграции.
