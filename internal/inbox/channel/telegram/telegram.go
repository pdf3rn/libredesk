// Package telegram implements a Telegram Bot API inbox that receives updates by long polling or webhook.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abhinavxd/libredesk/internal/conversation/models"
	"github.com/abhinavxd/libredesk/internal/stringutil"
	"github.com/zerodha/logf"
)

const ChannelTelegram = "telegram"

const (
	ModePolling = "polling"
	ModeWebhook = "webhook"
)

const (
	defaultPollTimeout = 30
	callTimeout        = 30 * time.Second
	sendTimeout        = 5 * time.Minute
	retryDelay         = 5 * time.Second
	webhookRetryDelay  = time.Minute
	maxUpdateAttempts  = 3
	maxTextLen         = 4096
	maxPhotoBytes      = 10 * 1024 * 1024
)

// Config is the inbox config stored in the DB.
type Config struct {
	BotToken      string `json:"bot_token"`
	Mode          string `json:"mode"`
	WebhookSecret string `json:"webhook_secret"`
	// PollTimeout is the long-poll wait in seconds (1-50).
	PollTimeout int `json:"poll_timeout,omitempty"`
	// APIURL overrides the Bot API endpoint (a local Bot API server); empty uses api.telegram.org.
	APIURL      string `json:"api_url,omitempty"`
	BotUsername string `json:"bot_username,omitempty"`
}

// EffectiveMode returns the receive mode, polling by default.
func (c Config) EffectiveMode() string {
	if c.Mode == ModeWebhook {
		return ModeWebhook
	}
	return ModePolling
}

// UpdateHandler ingests one update. A returned error asks for a retry.
type UpdateHandler func(ctx context.Context, inboxID int, cfg Config, u Update) error

type Opts struct {
	ID         int
	Name       string
	Config     Config
	Lo         *logf.Logger
	Handler    UpdateHandler
	WebhookURL string
}

type Telegram struct {
	id         int
	name       string
	cfg        Config
	lo         *logf.Logger
	client     *Client
	handler    UpdateHandler
	webhookURL string
}

func New(opts Opts) (*Telegram, error) {
	if opts.Config.BotToken == "" {
		return nil, errors.New("telegram bot token is empty")
	}
	if opts.Handler == nil {
		return nil, errors.New("telegram update handler is nil")
	}
	return &Telegram{
		id:         opts.ID,
		name:       opts.Name,
		cfg:        opts.Config,
		lo:         opts.Lo,
		client:     NewClient(opts.Config.BotToken, opts.Config.APIURL),
		handler:    opts.Handler,
		webhookURL: opts.WebhookURL,
	}, nil
}

func (t *Telegram) Identifier() int          { return t.id }
func (t *Telegram) Name() string             { return t.name }
func (t *Telegram) FromAddress() string      { return t.cfg.BotUsername }
func (t *Telegram) FromNameTemplate() string { return "" }
func (t *Telegram) ReplyToAddress() string   { return "" }
func (t *Telegram) Channel() string          { return ChannelTelegram }
func (t *Telegram) Close() error             { return nil }
func (t *Telegram) Client() *Client          { return t.client }

// Receive runs until ctx is cancelled: a getUpdates loop in polling mode, webhook registration in webhook mode.
func (t *Telegram) Receive(ctx context.Context) error {
	if t.cfg.EffectiveMode() == ModeWebhook {
		t.registerWebhook(ctx)
		<-ctx.Done()
		return nil
	}
	return t.poll(ctx)
}

func (t *Telegram) registerWebhook(ctx context.Context) {
	if t.webhookURL == "" {
		t.lo.Error("telegram webhook not registered: set a public HTTPS root URL in Settings and re-save the inbox", "inbox_id", t.id)
		return
	}
	for {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := t.client.SetWebhook(cctx, t.webhookURL, t.cfg.WebhookSecret)
		cancel()
		if err == nil {
			t.lo.Info("telegram webhook registered", "inbox_id", t.id, "url", t.webhookURL)
			return
		}
		t.lo.Error("error registering telegram webhook, retrying", "inbox_id", t.id, "url", t.webhookURL, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(webhookRetryDelay):
		}
	}
}

func (t *Telegram) poll(ctx context.Context) error {
	timeout := t.cfg.PollTimeout
	if timeout <= 0 || timeout > 50 {
		timeout = defaultPollTimeout
	}

	// getUpdates refuses to run while a webhook is set.
	for {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		err := t.client.DeleteWebhook(cctx)
		cancel()
		if err == nil {
			break
		}
		t.lo.Error("error clearing telegram webhook before polling, retrying", "inbox_id", t.id, "error", err)
		if !sleepCtx(ctx, retryDelay) {
			return nil
		}
	}
	t.lo.Info("telegram polling started", "inbox_id", t.id)

	var (
		offset   int64
		attempts = map[int64]int{}
	)
	for {
		if ctx.Err() != nil {
			return nil
		}
		cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+callTimeout)
		updates, err := t.client.GetUpdates(cctx, offset, timeout)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			delay := retryDelay
			var ae *APIError
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				delay = time.Duration(ae.RetryAfter) * time.Second
			}
			t.lo.Error("error polling telegram updates", "inbox_id", t.id, "error", err)
			if !sleepCtx(ctx, delay) {
				return nil
			}
			continue
		}

		for _, u := range updates {
			if err := t.handler(ctx, t.id, t.cfg, u); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				attempts[u.UpdateID]++
				if attempts[u.UpdateID] < maxUpdateAttempts {
					t.lo.Error("error processing telegram update, retrying", "inbox_id", t.id, "update_id", u.UpdateID, "attempt", attempts[u.UpdateID], "error", err)
					// Leave the offset at this update so it is fetched again.
					sleepCtx(ctx, retryDelay)
					break
				}
				t.lo.Error("dropping telegram update after repeated failures", "inbox_id", t.id, "update_id", u.UpdateID, "error", err)
			}
			delete(attempts, u.UpdateID)
			offset = u.UpdateID + 1
		}
	}
}

// Send delivers an agent reply. The chat id comes in To[0].
func (t *Telegram) Send(m models.OutboundMessage) error {
	if len(m.To) == 0 || m.To[0] == "" {
		return errors.New("telegram recipient chat id missing")
	}
	chatID := m.To[0]
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	text := strings.TrimSpace(outboundText(m))
	for _, chunk := range splitText(text, maxTextLen) {
		if _, err := t.client.SendMessage(ctx, chatID, chunk); err != nil {
			return fmt.Errorf("sending telegram message: %w", err)
		}
	}
	for _, att := range m.Attachments {
		kind := "document"
		ct := strings.ToLower(att.ContentType)
		if (strings.HasPrefix(ct, "image/jpeg") || strings.HasPrefix(ct, "image/png")) && len(att.Content) <= maxPhotoBytes {
			kind = "photo"
		}
		if _, err := t.client.SendFile(ctx, kind, chatID, att.Name, att.ContentType, att.Content, ""); err != nil {
			return fmt.Errorf("sending telegram attachment %q: %w", att.Name, err)
		}
	}
	return nil
}

func outboundText(m models.OutboundMessage) string {
	if m.ContentType == models.ContentTypeHTML && m.Content != "" {
		return stringutil.HTML2TextMarkdownLinks(m.Content)
	}
	if m.TextContent != "" {
		return m.TextContent
	}
	return m.Content
}

// splitText cuts text into pieces of at most n runes, preferring line breaks.
func splitText(s string, n int) []string {
	if s == "" {
		return nil
	}
	var out []string
	for utf8.RuneCountInString(s) > n {
		r := []rune(s)
		cut := n
		if i := strings.LastIndex(string(r[:n]), "\n"); i > 0 {
			cut = utf8.RuneCountInString(string(r[:n])[:i])
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		s = strings.TrimSpace(string(r[cut:]))
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
