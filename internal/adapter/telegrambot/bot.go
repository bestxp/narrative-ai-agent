package telegrambot

import (
	"fmt"
	"strings"

	tg "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/rs/zerolog"

	"narrative/internal/config"
)

// Bot is the Telegram-side adapter. It owns the whitelist, the routing
// table and the response sender. The actual GM logic lives in the
// usecase layer and is injected via the Handler interface — keeping
// the adapter thin and easy to swap in tests.
type Bot struct {
	api     *tg.BotAPI
	cfg     *config.Config
	handler Handler
	log     zerolog.Logger
}

type Handler interface {
	Handle(ctx *Context) (string, error)
}

type Context struct {
	UserID  int
	ChatID  int64
	Command string
	Args    []string
	RawText string
}

func New(token string, cfg *config.Config, handler Handler) (*Bot, error) {
	return NewWithLogger(token, cfg, handler, zerolog.Nop())
}

func NewWithLogger(token string, cfg *config.Config, handler Handler, log zerolog.Logger) (*Bot, error) {
	api, err := tg.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("tg init: %w", err)
	}
	return &Bot{
		api:     api,
		cfg:     cfg,
		handler: handler,
		log:     log.With().Str("component", "telegram").Logger(),
	}, nil
}

func (b *Bot) Run() error {
	u := tg.NewUpdate(0)
	u.Timeout = b.cfg.Telegram.PollingTimeout
	updates, err := b.api.GetUpdatesChan(u)
	if err != nil {
		return fmt.Errorf("get updates: %w", err)
	}
	b.log.Info().Str("username", b.api.Self.UserName).Msg("bot started")
	for upd := range updates {
		if upd.Message == nil {
			continue
		}
		if !b.cfg.IsAllowed(upd.Message.From.ID) {
			b.log.Warn().Int("user_id", upd.Message.From.ID).Msg("rejected user")
			continue
		}
		b.dispatch(upd.Message)
	}
	return nil
}

func (b *Bot) dispatch(m *tg.Message) {
	text := strings.TrimSpace(m.Text)
	ctx := &Context{
		UserID:  m.From.ID,
		ChatID:  m.Chat.ID,
		RawText: text,
	}
	if strings.HasPrefix(text, "/") {
		parts := strings.Fields(text)
		ctx.Command = strings.TrimPrefix(parts[0], "/")
		if len(parts) > 1 {
			ctx.Args = parts[1:]
		}
	}
	reply, err := b.handler.Handle(ctx)
	if err != nil {
		b.log.Error().Err(err).Int("user_id", m.From.ID).Msg("handler error")
		reply = "⚠️ " + err.Error()
	}
	if reply == "" {
		return
	}
	msg := tg.NewMessage(m.Chat.ID, reply)
	if b.cfg.Telegram.ParseMode != "" {
		msg.ParseMode = b.cfg.Telegram.ParseMode
	}
	if _, err := b.api.Send(msg); err != nil {
		b.log.Error().Err(err).Msg("send error")
	}
}

// Send allows the handler layer to send proactive messages (e.g. on
// scheduled maintenance). It respects the same parse mode and bypasses
// the routing table.
func (b *Bot) Send(chatID int64, body string) error {
	msg := tg.NewMessage(chatID, body)
	if b.cfg.Telegram.ParseMode != "" {
		msg.ParseMode = b.cfg.Telegram.ParseMode
	}
	_, err := b.api.Send(msg)
	return err
}