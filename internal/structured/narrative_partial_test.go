package structured_test

import (
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/structured"
)

func TestRenderAnyPartial_OnlyFilledSections(t *testing.T) {
	t.Parallel()

	raw := "------ NARRATIVE ------\n\nДень 24"

	got := structured.RenderAnyPartial(raw)
	if !strings.Contains(got, structured.HeaderDialogue) {
		t.Fatalf("missing dialogue header: %q", got)
	}

	emptyHeaders := []string{structured.HeaderContext, structured.HeaderFuture, structured.HeaderValidation}
	for _, h := range emptyHeaders {
		if strings.Contains(got, h) {
			t.Fatalf("unexpected empty section header %s in partial render: %q", h, got)
		}
	}
}

func TestRenderAnyPartial_GrowsAsSectionsArrive(t *testing.T) {
	t.Parallel()

	raw := "------ NARRATIVE ------\n\nДиалог\n\n------ CONTEXT ------\n\nКонтекст"

	got := structured.RenderAnyPartial(raw)
	if !strings.Contains(got, structured.HeaderDialogue) {
		t.Fatalf("missing dialogue header: %q", got)
	}

	if !strings.Contains(got, structured.HeaderContext) {
		t.Fatalf("missing context header: %q", got)
	}

	emptyHeaders := []string{structured.HeaderFuture, structured.HeaderValidation}
	for _, h := range emptyHeaders {
		if strings.Contains(got, h) {
			t.Fatalf("unexpected empty section header %s in partial render: %q", h, got)
		}
	}
}

func TestRenderAny_FinalIncludesAllSections(t *testing.T) {
	t.Parallel()

	sections := []string{
		"------ NARRATIVE ------",
		"\n\nДиалог",
		"\n\n------ CONTEXT ------",
		"\n\nКонтекст",
		"\n\n------ FUTURE ------",
		"\n\nБудущее",
		"\n\n------ SYSTEM ------",
		"\n\nПроверка",
	}

	got := structured.RenderAny(strings.Join(sections, ""))

	requiredHeaders := []string{
		structured.HeaderDialogue,
		structured.HeaderContext,
		structured.HeaderFuture,
		structured.HeaderValidation,
	}

	for _, h := range requiredHeaders {
		if !strings.Contains(got, h) {
			t.Fatalf("missing final header %s: %q", h, got)
		}
	}
}
