package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/abhinavxd/libredesk/internal/attachment"
	cmodels "github.com/abhinavxd/libredesk/internal/conversation/models"
	"github.com/abhinavxd/libredesk/internal/envelope"
	"github.com/abhinavxd/libredesk/internal/inbox"
	telegramChannel "github.com/abhinavxd/libredesk/internal/inbox/channel/telegram"
	imodels "github.com/abhinavxd/libredesk/internal/inbox/models"
	"github.com/abhinavxd/libredesk/internal/stringutil"
	umodels "github.com/abhinavxd/libredesk/internal/user/models"
	"github.com/valyala/fasthttp"
	"github.com/volatiletech/null/v9"
	"github.com/zerodha/fastglue"
)

const (
	telegramDefaultContactName = "Contact"
	telegramUpdateTimeout      = 3 * time.Minute
	telegramSecretHeader       = "X-Telegram-Bot-Api-Secret-Token"
)

var telegramLocks = &keyedLock{entries: make(map[string]*keyedLockEntry)}

// initTelegramInbox builds a Telegram inbox whose updates are ingested by processTelegramUpdate.
func initTelegramInbox(app *App, inboxRecord imodels.Inbox) (inbox.Inbox, error) {
	lo := initLogger("telegram_inbox")
	var cfg telegramChannel.Config
	if err := json.Unmarshal(inboxRecord.Config, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling telegram config for inbox %q: %w", inboxRecord.Name, err)
	}
	var hookURL string
	if app != nil && cfg.EffectiveMode() == telegramChannel.ModeWebhook {
		root, _ := app.setting.GetAppRootURL()
		if isPublicWebhookURL(root) {
			hookURL = telegramWebhookURLFromRoot(root, inboxRecord.ID)
		}
	}
	inb, err := telegramChannel.New(telegramChannel.Opts{
		ID:         inboxRecord.ID,
		Name:       inboxRecord.Name,
		Config:     cfg,
		Lo:         lo,
		WebhookURL: hookURL,
		Handler: func(ctx context.Context, inboxID int, cfg telegramChannel.Config, u telegramChannel.Update) error {
			if app == nil {
				return errors.New("app not ready")
			}
			return processTelegramUpdate(ctx, app, inboxID, cfg, u)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initializing `%s` inbox: `%s` error: %w", inboxRecord.Channel, inboxRecord.Name, err)
	}
	lo.Info("inbox initialized", "inbox_id", inboxRecord.ID, "channel", inboxRecord.Channel, "mode", cfg.EffectiveMode())
	return inb, nil
}

func telegramWebhookURLFromRoot(root string, inboxID int) string {
	if root == "" {
		return ""
	}
	return strings.TrimRight(root, "/") + "/webhooks/telegram/" + strconv.Itoa(inboxID)
}

// handleTelegramWebhook receives updates Telegram pushes to an inbox in webhook mode.
func handleTelegramWebhook(r *fastglue.Request) error {
	app := r.Context.(*App)
	inboxID, err := inboxIDFromPath(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "invalid inbox id", nil, envelope.InputError)
	}
	rec, err := app.inbox.GetDBRecord(inboxID)
	if err != nil || rec.Channel != telegramChannel.ChannelTelegram {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "inbox not found", nil, envelope.NotFoundError)
	}
	var cfg telegramChannel.Config
	if err := json.Unmarshal(rec.Config, &cfg); err != nil {
		app.lo.Error("error decoding telegram config", "inbox_id", inboxID, "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "invalid inbox config", nil, envelope.GeneralError)
	}
	got := r.RequestCtx.Request.Header.Peek(telegramSecretHeader)
	if cfg.WebhookSecret == "" || subtle.ConstantTimeCompare(got, []byte(cfg.WebhookSecret)) != 1 {
		app.lo.Warn("telegram webhook rejected: secret token mismatch", "inbox_id", inboxID)
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "invalid secret token", nil, envelope.PermissionError)
	}
	// A disabled inbox or one switched to polling acknowledges and drops, so Telegram stops redelivering.
	if !rec.Enabled || cfg.EffectiveMode() != telegramChannel.ModeWebhook {
		return r.SendEnvelope(map[string]string{"status": "ignored"})
	}

	var u telegramChannel.Update
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &u); err != nil {
		app.lo.Warn("dropping unparseable telegram update", "inbox_id", inboxID, "error", err)
		return r.SendEnvelope(map[string]string{"status": "ignored"})
	}

	ctx, cancel := context.WithTimeout(app.ctx, telegramUpdateTimeout)
	defer cancel()
	if err := processTelegramUpdate(ctx, app, inboxID, cfg, u); err != nil {
		app.lo.Error("error processing telegram update, asking telegram to retry", "inbox_id", inboxID, "update_id", u.UpdateID, "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "retry later", nil, envelope.GeneralError)
	}
	return r.SendEnvelope(map[string]string{"status": "ok"})
}

// processTelegramUpdate turns one Bot API update into a contact, conversation and incoming message.
func processTelegramUpdate(ctx context.Context, app *App, inboxID int, cfg telegramChannel.Config, u telegramChannel.Update) error {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot {
		return nil
	}
	// Only one-to-one chats map to a customer conversation.
	if m.Chat.Type != "private" {
		app.lo.Debug("ignoring telegram message from non-private chat", "inbox_id", inboxID, "chat_type", m.Chat.Type)
		return nil
	}

	chatID := strconv.FormatInt(m.Chat.ID, 10)
	sourceID := "telegram:" + chatID + ":" + strconv.FormatInt(m.MessageID, 10)

	defer telegramLocks.lock(chatID + ":" + strconv.Itoa(inboxID))()

	if exists, err := app.conversation.MessageExists(sourceID); err != nil {
		return fmt.Errorf("checking duplicate: %w", err)
	} else if exists {
		return nil
	}

	inbRec, err := app.inbox.GetDBRecord(inboxID)
	if err != nil {
		return fmt.Errorf("fetching inbox: %w", err)
	}
	if !inbRec.Enabled {
		return nil
	}

	contactID, err := upsertTelegramContact(app, chatID, m.From)
	if err != nil {
		return fmt.Errorf("resolving contact: %w", err)
	}
	contact, err := app.user.GetContactOrVisitor(contactID, "" /** email **/)
	if err != nil {
		return fmt.Errorf("checking contact: %w", err)
	}
	if !contact.Enabled {
		return nil
	}

	attachments := fetchTelegramAttachments(ctx, app, cfg, m)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	preview := telegramTextPreview(m)
	isNewConversation := false
	conversationID, conversationUUID, err := app.conversation.GetLatestOpenConversationForContact(contactID, inboxID)
	if errors.Is(err, sql.ErrNoRows) && inbRec.ReopenWindowHours > 0 {
		conversationID, conversationUUID, err = app.conversation.GetReopenableConversationForContact(contactID, inboxID, inbRec.ReopenWindowHours)
	}
	if errors.Is(err, sql.ErrNoRows) {
		conversationID, conversationUUID, err = app.conversation.CreateConversation(
			contactID,
			inboxID,
			preview,    /** last_message **/
			time.Now(), /** last_message_at **/
			"",         /** subject **/
			false,      /** append_ref_num_to_subject **/
			nil,        /** meta **/
			nil,        /** custom_attributes **/
			0,          /** max_conversations **/
			0,          /** rate_limit_window **/
		)
		if err != nil {
			return fmt.Errorf("creating conversation: %w", err)
		}
		isNewConversation = true
	} else if err != nil {
		return fmt.Errorf("looking up conversation: %w", err)
	}

	content := preview
	if m.Text == "" && m.Caption == "" && len(attachments) > 0 {
		content = ""
	}

	msg := cmodels.Message{
		Channel:          telegramChannel.ChannelTelegram,
		ConversationID:   conversationID,
		ConversationUUID: conversationUUID,
		SenderID:         contactID,
		SenderType:       cmodels.SenderTypeContact,
		Type:             cmodels.MessageIncoming,
		Status:           cmodels.MessageStatusReceived,
		InboxID:          inboxID,
		Content:          content,
		ContentType:      cmodels.ContentTypeText,
		SourceID:         null.StringFrom(sourceID),
		Attachments:      attachments,
	}
	if _, err := app.conversation.ProcessIncomingTelegramMessage(msg, isNewConversation); err != nil {
		return fmt.Errorf("processing telegram message: %w", err)
	}
	return nil
}

func upsertTelegramContact(app *App, chatID string, from *telegramChannel.User) (int, error) {
	first, last := from.FirstName, from.LastName
	if first == "" {
		first = from.Username
	}
	if first == "" {
		first = telegramDefaultContactName
	}
	contact := umodels.User{
		Type:      umodels.UserTypeContact,
		FirstName: first,
		LastName:  last,
	}
	id, err := app.user.UpsertContactByChannelIdentity(telegramChannel.ChannelTelegram, chatID, &contact)
	if err != nil {
		return 0, err
	}
	if first != telegramDefaultContactName {
		if err := app.user.UpdateContactNameIfDefault(id, first, last, telegramDefaultContactName); err != nil {
			app.lo.Error("error updating telegram contact name", "user_id", id, "error", err)
		}
	}
	return id, nil
}

// fetchTelegramAttachments downloads the message's file. Failures leave the text placeholder instead of blocking the message.
func fetchTelegramAttachments(ctx context.Context, app *App, cfg telegramChannel.Config, m *telegramChannel.Message) attachment.Attachments {
	ref, kind, ok := m.Media()
	if !ok || ref.FileID == "" {
		return nil
	}
	maxBytes := int64(app.consts.Load().(*constants).MaxFileUploadSizeMB) * 1024 * 1024
	if maxBytes <= 0 || maxBytes > telegramChannel.MaxDownloadBytes {
		maxBytes = telegramChannel.MaxDownloadBytes
	}
	client := telegramChannel.NewClient(cfg.BotToken, cfg.APIURL)
	dlCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	body, filePath, err := client.DownloadFile(dlCtx, ref.FileID, maxBytes)
	if err != nil {
		app.lo.Warn("could not download telegram file, storing placeholder", "kind", kind, "error", err)
		return nil
	}
	if len(body) == 0 {
		return nil
	}
	name := ref.FileName
	if name == "" {
		name = path.Base(filePath)
	}
	if name == "" || name == "." || name == "/" {
		name = defaultMediaFilename(kind, ref.MimeType)
	}
	contentType := ref.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return attachment.Attachments{
		attachment.Attachment{
			Name:        name,
			ContentType: contentType,
			Content:     body,
			Size:        len(body),
			Disposition: attachment.DispositionAttachment,
		},
	}
}

func telegramTextPreview(m *telegramChannel.Message) string {
	if m.Text != "" {
		return m.Text
	}
	if m.Caption != "" {
		return m.Caption
	}
	switch {
	case len(m.Photo) > 0:
		return "[image]"
	case m.Video != nil || m.VideoNote != nil:
		return "[video]"
	case m.Voice != nil || m.Audio != nil:
		return "[audio]"
	case m.Document != nil:
		return "[document]"
	case m.Sticker != nil:
		return "[sticker]"
	case m.Location != nil:
		return fmt.Sprintf("[location] https://maps.google.com/?q=%f,%f", m.Location.Latitude, m.Location.Longitude)
	case m.Contact != nil:
		return strings.TrimSpace(fmt.Sprintf("[contact] %s %s %s", m.Contact.FirstName, m.Contact.LastName, m.Contact.PhoneNumber))
	}
	return "[telegram message]"
}

// prepareTelegramInbox fills in defaults, checks the bot token with Telegram and requires a public URL for webhook mode.
func prepareTelegramInbox(r *fastglue.Request, app *App, inb *imodels.Inbox) error {
	var cfg telegramChannel.Config
	if err := json.Unmarshal(inb.Config, &cfg); err != nil {
		return envelope.NewError(envelope.InputError, app.i18n.T("validation.invalidValue"), nil)
	}
	cfg.BotToken = strings.TrimSpace(cfg.BotToken)
	cfg.APIURL = strings.TrimSpace(cfg.APIURL)
	if cfg.Mode != telegramChannel.ModeWebhook {
		cfg.Mode = telegramChannel.ModePolling
	}
	if cfg.BotToken == "" || strings.Contains(cfg.BotToken, stringutil.PasswordDummy) {
		return envelope.NewError(envelope.InputError, app.i18n.Ts("globals.messages.required", "name", "`bot_token`"), nil)
	}
	if cfg.Mode == telegramChannel.ModeWebhook {
		root, _ := app.setting.GetAppRootURL()
		if !isPublicWebhookURL(root) {
			return envelope.NewError(envelope.InputError, "Webhook mode needs the app root URL (Settings → General) to be a public HTTPS address Telegram can reach. Use polling mode otherwise.", nil)
		}
		if cfg.WebhookSecret == "" || strings.Contains(cfg.WebhookSecret, stringutil.PasswordDummy) {
			secret, err := stringutil.RandomAlphanumeric(48)
			if err != nil {
				return envelope.NewError(envelope.GeneralError, app.i18n.T("globals.messages.somethingWentWrong"), nil)
			}
			cfg.WebhookSecret = secret
		}
	}
	ctx, cancel := context.WithTimeout(r.RequestCtx, 20*time.Second)
	defer cancel()
	me, err := telegramChannel.NewClient(cfg.BotToken, cfg.APIURL).GetMe(ctx)
	if err != nil {
		return envelope.NewError(envelope.InputError, "Telegram rejected the bot token: "+err.Error(), nil)
	}
	cfg.BotUsername = me.Username
	if inb.From == "" && me.Username != "" {
		inb.From = "@" + me.Username
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	inb.Config = raw
	return nil
}
