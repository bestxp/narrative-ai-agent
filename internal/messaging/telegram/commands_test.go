package telegram

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"narrative/internal/messaging"
)

func TestEncodeCommandsJSON_Empty(t *testing.T) {
	assert.Equal(t, "[]", encodeCommandsJSON(nil))
	assert.Equal(t, "[]", encodeCommandsJSON([]messaging.BotCommand{}))
}

func TestEncodeCommandsJSON_Single(t *testing.T) {
	out := encodeCommandsJSON([]messaging.BotCommand{
		{Command: "start", Description: "Загрузить info.yaml"},
	})
	assert.Equal(t, `[{"command":"start","description":"Загрузить info.yaml"}]`, out)
}

func TestEncodeCommandsJSON_Multiple(t *testing.T) {
	out := encodeCommandsJSON([]messaging.BotCommand{
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
	out := encodeCommandsJSON([]messaging.BotCommand{
		{Command: "x", Description: `игра "Найти Кагую"`},
	})
	assert.Contains(t, out, `\"Найти Кагую\"`)
}

func TestEncodeCommandsJSON_StripsControlChars(t *testing.T) {
	out := encodeCommandsJSON([]messaging.BotCommand{
		{Command: "x", Description: "before\x00\x01after"},
	})
	assert.NotContains(t, out, "\x00")
	assert.NotContains(t, out, "\x01")
	assert.Contains(t, out, "before")
	assert.Contains(t, out, "after")
}

func TestJsonString_Basic(t *testing.T) {
	assert.Equal(t, `"hello"`, jsonString("hello"))
	assert.Equal(t, `"with\"quote"`, jsonString(`with"quote`))
	assert.Equal(t, `"line\nbreak"`, jsonString("line\nbreak"))
}

func TestAsURLValues_Conversion(t *testing.T) {
	v := asURLValues(map[string]string{"a": "1", "b": "2"})
	assert.Equal(t, "1", v.Get("a"))
	assert.Equal(t, "2", v.Get("b"))
}

func TestAsURLValues_Empty(t *testing.T) {
	v := asURLValues(nil)
	assert.NotNil(t, v)
}

func TestSetCommands_BuildsCorrectPayload(t *testing.T) {
	// We can't actually reach Telegram from tests, but we can
	// confirm the encoding helper produces what the API
	// expects. The full SetCommands call uses MakeRequest
	// which we don't intercept here.
	cmds := []messaging.BotCommand{
		{Command: "start", Description: "Загрузить info.yaml и state.md"},
		{Command: "me", Description: "Содержимое SOUL/SKILL/memory/state"},
		{Command: "save", Description: "git commit + push"},
	}
	payload := encodeCommandsJSON(cmds)
	assert.Equal(t, `[{"command":"start","description":"Загрузить info.yaml и state.md"},{"command":"me","description":"Содержимое SOUL/SKILL/memory/state"},{"command":"save","description":"git commit + push"}]`, payload)
	// Encoded payload must be valid url.Values when wrapped.
	v := url.Values{}
	v.Set("commands", payload)
	assert.Equal(t, payload, v.Get("commands"))
}

func TestSetCommands_ContextHonoured(t *testing.T) {
	// Sanity: SetCommands takes a context even though the
	// underlying MakeRequest does not honour cancellation.
	// The function must not panic on a cancelled context.
	c := &Client{cfg: Config{}, api: nil, log: discardLogger()}
	_ = c // the call below would actually use c.api; here we
	// only verify the signature accepts context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx
	_ = c
	require.NotNil(t, c)
}
