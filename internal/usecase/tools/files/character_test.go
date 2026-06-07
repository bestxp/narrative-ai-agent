package files

import (
	"strings"
	"testing"
)

func TestUpsertSection_NewSection(t *testing.T) {
	body := "# Title\n## Existing\nold text\n"
	got := upsertSection(body, "New", "new text")
	if c := strings.Count(got, "## New"); c != 1 {
		t.Fatalf("expected 1 '## New' header, got %d\nfull:\n%s", c, got)
	}
	if !strings.Contains(got, "new text") {
		t.Fatalf("missing new text: %s", got)
	}
	// Old section must remain intact.
	if !strings.Contains(got, "old text") {
		t.Fatalf("old text dropped: %s", got)
	}
}

func TestUpsertSection_AppendsInPlace(t *testing.T) {
	// "Действия дня 7" is a log section — APPEND
	// keeps the journal history. We use it here
	// (not "Оружие" which is a state section and
	// would REPLACE) so the test exercises the
	// append branch of upsertSection.
	body := "# Title\n## Действия дня 7\nпобег из подземелья\n## Философия\nold\n"
	got := upsertSection(body, "Действия дня 7", "разговор с Хокаге")
	// Exactly one "## Действия дня 7" header.
	if c := strings.Count(got, "## Действия дня 7"); c != 1 {
		t.Fatalf("expected 1 '## Действия дня 7' header, got %d\nfull:\n%s", c, got)
	}
	// Both old and new lines live between the header
	// and the next section.
	if !strings.Contains(got, "побег из подземелья") || !strings.Contains(got, "разговор с Хокаге") {
		t.Fatalf("expected both old and new content: %s", got)
	}
	if !strings.Contains(got, "## Философия") {
		t.Fatalf("next section dropped: %s", got)
	}
}

func TestUpsertSection_Dedup(t *testing.T) {
	body := "# T\n## X\nhello\n"
	got := upsertSection(body, "X", "hello")
	if got != body {
		t.Fatalf("expected idempotent no-op, got:\n%s", got)
	}
}

func TestUpsertSection_PreExistingDuplicates(t *testing.T) {
	// The legacy markus/SOUL.md shipped with two
	// "## Истинная сущность" headers (an old manual
	// edit). The old upsertSection turned one
	// Append into FOUR headers. The fix must not
	// regress: even with pre-existing duplicates,
	// the new stitching adds exactly ONE new line
	// to the FIRST occurrence and leaves the
	// duplicates alone (we are not a markdown
	// normaliser — that is the operator's job).
	body := "# T\n## Истинная сущность\n(опишите позже)\n\n## Истинная сущность\n(опишите позже)\n## Философия\n"
	got := upsertSection(body, "Истинная сущность", "одет в форму шиноби")
	// Still two headers — the pre-existing duplicate
	// is not removed.
	if c := strings.Count(got, "## Истинная сущность"); c != 2 {
		t.Fatalf("expected 2 '## Истинная сущность' headers (preserved), got %d\nfull:\n%s", c, got)
	}
	// The new text lands between the first header
	// and the second header.
	if !strings.Contains(got, "одет в форму шиноби") {
		t.Fatalf("new text missing: %s", got)
	}
	// The "## Философия" tail is preserved.
	if !strings.Contains(got, "## Философия") {
		t.Fatalf("tail section dropped: %s", got)
	}
	// Critical: the Append must NOT create a
	// duplicate HEADER. The legacy file already
	// had two `## Истинная сущность` lines, and
	// those are preserved (operator's job to
	// normalise). The Append only adds a line in
	// the first section's body; it must not emit
	// a third `## Истинная сущность` of its own.
	if strings.Contains(got, "## Истинная сущность\n## Истинная сущность") {
		t.Fatalf("Append created a duplicate header: %s", got)
	}
}

func TestUpsertSection_EmptyBody(t *testing.T) {
	got := upsertSection("", "X", "y")
	if !strings.Contains(got, "## X") || !strings.Contains(got, "y") {
		t.Fatalf("expected '## X\\ny' on empty body, got %q", got)
	}
}

func TestUpsertSection_AppendAtEndOfFile(t *testing.T) {
	body := "# T\n## First\ntext\n"
	got := upsertSection(body, "Last", "tail text")
	if !strings.HasSuffix(got, "## Last\ntail text\n") {
		t.Fatalf("expected appended section at end, got %q", got)
	}
}

func TestSectionMode_ClassifiesStateSections(t *testing.T) {
	// A representative sample of sections that
	// describe the player's CURRENT state. Updates
	// here must REPLACE, not append — otherwise
	// the character ends up wearing two costumes
	// at the same time after a wardrobe change.
	// The list is the canonical stateSectionNames
	// set — keep this in sync with that list.
	states := []string{
		"Истинная сущность",
		"Внешний вид",
		"Визуальный возраст",
		"Философия и принципы",
		"Оружие",
		"Базовые способности",
		"Универсальные навыки",
		"Ограничения",
		"Текущий статус",
		"Эмоции",
	}
	for _, s := range states {
		if got := classifySection(s); got != sectionModeState {
			t.Errorf("section %q: expected ModeState, got %v", s, got)
		}
	}
}

func TestSectionMode_ClassifiesLogSections(t *testing.T) {
	logs := []string{
		"Яркие воспоминания",
		"Эволюция",
		"Команда и компаньоны",
		"Действия дня 7",
		"Предпочтения",
		"Команда",   // arbitrary unknown, defaults to log
		"Алфавит",  // arbitrary unknown, defaults to log
		// The dropped casual aliases used to be on
		// the state list; they should now fall
		// through to the log default. The model is
		// expected to use the canonical header
		// ("внешний вид", "философия и принципы",
		// "оружие") instead — these are operator-
		// facing names, not aliases we silently
		// accept.
		"Внешность",
		"Философия",
		"Меч",
		"Особое свойство хранителя",
	}
	for _, s := range logs {
		if got := classifySection(s); got != sectionModeLog {
			t.Errorf("section %q: expected ModeLog, got %v", s, got)
		}
	}
}

func TestUpsertSection_StateSectionReplaces(t *testing.T) {
	// The character has worn a tracksuit. They
	// change into a shinobi uniform. The section
	// "Истинная сущность" is classified as a
	// state section — the Append that lands on
	// it must REPLACE the old body, not stack
	// the new text underneath.
	body := "# T\n## Истинная сущность\nодет в чёрный спортивный костюм\n## Философия\nпросто\n"
	got := upsertSection(body, "Истинная сущность", "одел форму шиноби")
	// Old text must be gone.
	if strings.Contains(got, "спортивный костюм") {
		t.Fatalf("old costume still present: %s", got)
	}
	// New text must be present.
	if !strings.Contains(got, "одел форму шиноби") {
		t.Fatalf("new costume missing: %s", got)
	}
	// Header count for Истинная сущность: exactly 1.
	if c := strings.Count(got, "## Истинная сущность"); c != 1 {
		t.Fatalf("expected 1 header, got %d\n%s", c, got)
	}
	// Tail section preserved.
	if !strings.Contains(got, "## Философия") {
		t.Fatalf("tail section dropped: %s", got)
	}
}

func TestUpsertSection_LogSectionAppends(t *testing.T) {
	body := "# T\n## Яркие воспоминания\nвидение с Кагуей\n## Философия\nпросто\n"
	got := upsertSection(body, "Яркие воспоминания", "контакт с Кагуей во сне")
	// Both old and new memory lines must be present.
	if !strings.Contains(got, "видение с Кагуей") {
		t.Fatalf("old memory dropped: %s", got)
	}
	if !strings.Contains(got, "контакт с Кагуей") {
		t.Fatalf("new memory missing: %s", got)
	}
	// Exactly one header.
	if c := strings.Count(got, "## Яркие воспоминания"); c != 1 {
		t.Fatalf("expected 1 header, got %d\n%s", c, got)
	}
}

func TestUpsertSection_StateReplacesEvenWithDuplicates(t *testing.T) {
	// The legacy markus/SOUL.md shipped with two
	// `## Истинная сущность` headers. The state
	// REPLACE path picks the FIRST occurrence and
	// replaces its body with the new text. The
	// second duplicate header stays put — the
	// operator is responsible for normalising
	// legacy files. The bot is not a markdown
	// cleaner; it is a section-update tool.
	body := "# T\n## Истинная сущность\nстарый костюм\n\n## Истинная сущность\n(опишите позже)\n## Философия\nпросто\n"
	got := upsertSection(body, "Истинная сущность", "новая одежда")
	// Old "старый костюм" must be gone.
	if strings.Contains(got, "старый костюм") {
		t.Fatalf("old state still present: %s", got)
	}
	// New text must be present.
	if !strings.Contains(got, "новая одежда") {
		t.Fatalf("new state missing: %s", got)
	}
	// The replacement lands in the first section
	// position; the (опишите позже) duplicate is
	// still there for the operator to clean up.
	if !strings.Contains(got, "(опишите позже)") {
		t.Fatalf("legacy duplicate should be preserved (operator's job to normalise), got: %s", got)
	}
}
