package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhinavxd/libredesk/internal/attachment"
	"github.com/abhinavxd/libredesk/internal/conversation/models"
	"github.com/zerodha/logf"
)

// fakeBotAPI is a minimal stand-in for the Telegram Bot API.
type fakeBotAPI struct {
	mu          sync.Mutex
	calls       []string
	sent        []map[string]any
	files       []string
	webhookURL  string
	webhookTok  string
	updatesSent bool
}

func (f *fakeBotAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/botTOKEN/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, `{"ok":false,"error_code":404,"description":"Not Found"}`, http.StatusNotFound)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, prefix)
		f.mu.Lock()
		f.calls = append(f.calls, method)
		f.mu.Unlock()
		reply := func(result any) {
			b, _ := json.Marshal(map[string]any{"ok": true, "result": result})
			w.Write(b)
		}
		switch method {
		case "deleteWebhook":
			reply(true)
		case "setWebhook":
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.webhookURL, _ = p["url"].(string)
			f.webhookTok, _ = p["secret_token"].(string)
			f.mu.Unlock()
			reply(true)
		case "getUpdates":
			f.mu.Lock()
			first := !f.updatesSent
			f.updatesSent = true
			f.mu.Unlock()
			if first {
				reply([]Update{{UpdateID: 7, Message: &Message{MessageID: 1, From: &User{ID: 42, FirstName: "Ana"}, Chat: Chat{ID: 42, Type: "private"}, Text: "hola"}}})
				return
			}
			time.Sleep(20 * time.Millisecond)
			reply([]Update{})
		case "sendMessage":
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.sent = append(f.sent, p)
			f.mu.Unlock()
			reply(Message{MessageID: 99})
		case "sendPhoto", "sendDocument":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("bad multipart: %v", err)
			}
			_, fh, err := r.FormFile(map[string]string{"sendPhoto": "photo", "sendDocument": "document"}[method])
			if err != nil {
				t.Errorf("missing file part: %v", err)
			} else {
				f.mu.Lock()
				f.files = append(f.files, method+":"+fh.Filename+":"+r.FormValue("chat_id"))
				f.mu.Unlock()
			}
			reply(Message{MessageID: 100})
		case "getMe":
			reply(User{ID: 1, IsBot: true, Username: "test_bot"})
		default:
			io.WriteString(w, `{"ok":false,"error_code":400,"description":"unknown method"}`)
		}
	})
}

func newTestInbox(t *testing.T, srv *httptest.Server, cfg Config, h UpdateHandler, hook string) *Telegram {
	t.Helper()
	cfg.BotToken = "TOKEN"
	cfg.APIURL = srv.URL
	lo := logf.New(logf.Opts{Level: logf.ErrorLevel})
	inb, err := New(Opts{ID: 5, Name: "tg", Config: cfg, Lo: &lo, Handler: h, WebhookURL: hook})
	if err != nil {
		t.Fatal(err)
	}
	return inb
}

func TestPollingDeliversUpdates(t *testing.T) {
	fake := &fakeBotAPI{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	got := make(chan Update, 1)
	inb := newTestInbox(t, srv, Config{Mode: ModePolling, PollTimeout: 1}, func(_ context.Context, inboxID int, _ Config, u Update) error {
		if inboxID != 5 {
			t.Errorf("inbox id = %d", inboxID)
		}
		got <- u
		return nil
	}, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inb.Receive(ctx); close(done) }()

	select {
	case u := <-got:
		if u.Message == nil || u.Message.Text != "hola" || u.Message.Chat.ID != 42 {
			t.Fatalf("unexpected update: %+v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no update delivered")
	}
	cancel()
	<-done

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 || fake.calls[0] != "deleteWebhook" {
		t.Fatalf("polling must clear the webhook first, calls = %v", fake.calls)
	}
}

func TestWebhookModeRegistersWebhook(t *testing.T) {
	fake := &fakeBotAPI{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	inb := newTestInbox(t, srv, Config{Mode: ModeWebhook, WebhookSecret: "s3cret"}, func(context.Context, int, Config, Update) error { return nil }, "https://desk.example.com/webhooks/telegram/5")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { inb.Receive(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		fake.mu.Lock()
		url, tok := fake.webhookURL, fake.webhookTok
		fake.mu.Unlock()
		if url != "" {
			if url != "https://desk.example.com/webhooks/telegram/5" || tok != "s3cret" {
				t.Fatalf("webhook = %q token = %q", url, tok)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("webhook was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestSendTextAndAttachments(t *testing.T) {
	fake := &fakeBotAPI{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	inb := newTestInbox(t, srv, Config{}, func(context.Context, int, Config, Update) error { return nil }, "")

	err := inb.Send(models.OutboundMessage{
		To:          []string{"42"},
		Content:     "<p>Hello <b>there</b></p>",
		ContentType: models.ContentTypeHTML,
		Attachments: attachment.Attachments{
			{Name: "pic.png", ContentType: "image/png", Content: []byte("png")},
			{Name: "doc.pdf", ContentType: "application/pdf", Content: []byte("pdf")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.sent) != 1 || fake.sent[0]["chat_id"] != "42" || !strings.Contains(fake.sent[0]["text"].(string), "Hello") || strings.Contains(fake.sent[0]["text"].(string), "<b>") {
		t.Fatalf("unexpected text sends: %v", fake.sent)
	}
	want := []string{"sendPhoto:pic.png:42", "sendDocument:doc.pdf:42"}
	if strings.Join(fake.files, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", fake.files, want)
	}
}

func TestSendNeedsChatID(t *testing.T) {
	fake := &fakeBotAPI{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	inb := newTestInbox(t, srv, Config{}, func(context.Context, int, Config, Update) error { return nil }, "")
	if err := inb.Send(models.OutboundMessage{Content: "x"}); err == nil {
		t.Fatal("expected an error without a chat id")
	}
}

func TestSplitText(t *testing.T) {
	long := strings.Repeat("a", 30) + "\n" + strings.Repeat("b", 30)
	parts := splitText(long, 40)
	if len(parts) != 2 || parts[0] != strings.Repeat("a", 30) || parts[1] != strings.Repeat("b", 30) {
		t.Fatalf("parts = %q", parts)
	}
	if got := splitText("", 10); got != nil {
		t.Fatalf("empty text should give no parts, got %q", got)
	}
	if got := splitText("ñandú", 10); len(got) != 1 || got[0] != "ñandú" {
		t.Fatalf("got %q", got)
	}
}

func TestMediaPicksLargestPhoto(t *testing.T) {
	m := Message{Photo: []PhotoSize{{FileID: "small"}, {FileID: "big"}}}
	ref, kind, ok := m.Media()
	if !ok || kind != "photo" || ref.FileID != "big" {
		t.Fatalf("got %+v %q %v", ref, kind, ok)
	}
	if _, _, ok := (&Message{Text: "hi"}).Media(); ok {
		t.Fatal("text message has no media")
	}
}
