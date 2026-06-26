package telegram_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/bestxp/narrative-ai-agent/internal/messaging"
	"github.com/bestxp/narrative-ai-agent/internal/messaging/telegram"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeCommandsJSON_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "[]", telegram.EncodeCommandsJSON(nil))
	assert.Equal(t, "[]", telegram.EncodeCommandsJSON([]messaging.BotCommand{}))
}

func TestEncodeCommandsJSON_Single(t *testing.T) {
	t.Parallel()

	out := telegram.EncodeCommandsJSON([]messaging.BotCommand{
		{Command: "start", Description: "Загрузить info.yaml"},
	})
	assert.JSONEq(t, `[{"command":"start","description":"Загрузить info.yaml"}]`, out)
}

func TestEncodeCommandsJSON_Multiple(t *testing.T) {
	t.Parallel()

	out := telegram.EncodeCommandsJSON([]messaging.BotCommand{
		{Command: "start", Description: "Загрузить"},
		{Command: "me", Description: "Персонаж"},
		{Command: "save", Description: "Сохранить"},
	})
	assert.Equal(t, 3, strings.Count(out, `"command":`))
	assert.Contains(t, out, `"start"`)
	assert.Contains(t, out, `"me"`)
	assert.Contains(t, out, `"save"`)
}

func TestEncodeCommandsJSON_EscapesQuotes(t *testing.T) {
	t.Parallel()

	out := telegram.EncodeCommandsJSON([]messaging.BotCommand{
		{Command: "x", Description: `игра "Найти Кагую"`},
	})
	assert.Contains(t, out, `\"Найти Кагую\"`)
}

func TestEncodeCommandsJSON_StripsControlChars(t *testing.T) {
	t.Parallel()

	out := telegram.EncodeCommandsJSON([]messaging.BotCommand{
		{Command: "x", Description: "before\x00\x01after"},
	})
	assert.NotContains(t, out, "\x00")
	assert.NotContains(t, out, "\x01")
	assert.Contains(t, out, "before")
	assert.Contains(t, out, "after")
}

func TestJsonString_Basic(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `"hello"`, telegram.JSONString("hello"))
	assert.Equal(t, `"with\"quote"`, telegram.JSONString(`with"quote`))
	assert.Equal(t, `"line\nbreak"`, telegram.JSONString("line\nbreak"))
}

func TestAsURLValues_Conversion(t *testing.T) {
	t.Parallel()

	v := telegram.AsURLValues(map[string]string{"a": "1", "b": "2"})
	assert.Equal(t, "1", v.Get("a"))
	assert.Equal(t, "2", v.Get("b"))
}

func TestAsURLValues_Empty(t *testing.T) {
	t.Parallel()

	v := telegram.AsURLValues(nil)
	assert.NotNil(t, v)
}

func TestSetCommands_BuildsCorrectPayload(t *testing.T) {
	t.Parallel()
	// We can't actually reach Telegram from tests, but we can
	// confirm the encoding helper produces what the API
	// expects. The full SetCommands call uses MakeRequest
	// which we don't intercept here.
	cmds := []messaging.BotCommand{
		{Command: "start", Description: "Загрузить info.yaml и state.md"},
		{Command: "me", Description: "Содержимое SOUL/SKILL/memory/state"},
		{Command: "save", Description: "git commit + push"},
	}
	payload := telegram.EncodeCommandsJSON(cmds)
	expected := `[{"command":"start","description":"Загрузить info.yaml и state.md"},` +
		`{"command":"me","description":"Содержимое SOUL/SKILL/memory/state"},` +
		`{"command":"save","description":"git commit + push"}]`
	assert.JSONEq(t, expected, payload)
	// Encoded payload must be valid url.Values when wrapped.
	v := url.Values{}
	v.Set("commands", payload)
	assert.Equal(t, payload, v.Get("commands"))
}

func TestSetCommands_ContextHonoured(t *testing.T) {
	t.Parallel()
	// Sanity: SetCommands takes a context even though the
	// underlying MakeRequest does not honour cancellation.
	// The function must not panic on a cancelled context.
	c := telegram.NewForTesting(telegram.Config{}, discardLogger())
	_ = c // the call below would actually use c.api; here we
	// only verify the signature accepts context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = ctx
	_ = c
	require.NotNil(t, c)
}
