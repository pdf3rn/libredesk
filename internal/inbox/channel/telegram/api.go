package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is the public Telegram Bot API endpoint.
const DefaultAPIURL = "https://api.telegram.org"

// MaxDownloadBytes is the Bot API getFile download limit.
const MaxDownloadBytes = 20 * 1024 * 1024

// ErrTooLarge is returned when a file exceeds the download cap.
var ErrTooLarge = errors.New("telegram file too large")

// APIError is a non-ok Bot API reply.
type APIError struct {
	StatusCode  int
	ErrorCode   int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.ErrorCode, e.Description)
}

// Client talks to the Telegram Bot API for a single bot token.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

// NewClient returns a Bot API client. apiURL may be empty for the public API.
func NewClient(token, apiURL string) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Client{
		token:   token,
		baseURL: strings.TrimRight(apiURL, "/"),
		// Per-request deadlines come from contexts; this only guards stuck connections.
		http: &http.Client{Timeout: 5 * time.Minute},
	}
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) methodURL(method string) string {
	return c.baseURL + "/bot" + c.token + "/" + method
}

// call posts a JSON body to a Bot API method and decodes the result into out.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	var body io.Reader
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// Never leak the token embedded in the request URL.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return fmt.Errorf("telegram request failed: %w", uerr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return err
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return &APIError{StatusCode: resp.StatusCode, Description: fmt.Sprintf("unexpected response (HTTP %d)", resp.StatusCode)}
	}
	if !ar.OK {
		return &APIError{StatusCode: resp.StatusCode, ErrorCode: ar.ErrorCode, Description: ar.Description, RetryAfter: ar.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

// GetMe validates the token and returns the bot's own user.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", nil, &u)
	return u, err
}

// GetUpdates long-polls for new updates.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSecs int) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSecs,
		"allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}

// SetWebhook points the bot at a public HTTPS URL, protected by a secret token header.
func (c *Client) SetWebhook(ctx context.Context, hookURL, secret string) error {
	return c.call(ctx, "setWebhook", map[string]any{
		"url":             hookURL,
		"secret_token":    secret,
		"allowed_updates": []string{"message"},
	}, nil)
}

// DeleteWebhook removes any webhook so getUpdates works. Pending updates are kept.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

// SendMessage sends a plain text message.
func (c *Client) SendMessage(ctx context.Context, chatID, text string) (Message, error) {
	var m Message
	err := c.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    text,
	}, &m)
	return m, err
}

// SendFile uploads a file with sendPhoto (method "photo") or sendDocument (method "document").
func (c *Client) SendFile(ctx context.Context, kind, chatID, filename, contentType string, content []byte, caption string) (Message, error) {
	method := "sendDocument"
	if kind == "photo" {
		method = "sendPhoto"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", chatID)
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name=%q; filename=%q`, kind, sanitizeFilename(filename))}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	h["Content-Type"] = []string{contentType}
	part, err := w.CreatePart(h)
	if err != nil {
		return Message{}, err
	}
	if _, err := part.Write(content); err != nil {
		return Message{}, err
	}
	if err := w.Close(); err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), &buf)
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var m Message
	err = c.do(req, &m)
	return m, err
}

// DownloadFile resolves a file_id and downloads its bytes, refusing anything over maxBytes.
func (c *Client) DownloadFile(ctx context.Context, fileID string, maxBytes int64) ([]byte, string, error) {
	var f File
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return nil, "", err
	}
	if maxBytes > 0 && f.FileSize > maxBytes {
		return nil, "", ErrTooLarge
	}
	if f.FilePath == "" {
		return nil, "", fmt.Errorf("telegram returned no file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/file/bot"+c.token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, "", fmt.Errorf("telegram download failed: %w", uerr.Err)
		}
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &APIError{StatusCode: resp.StatusCode, Description: "file download failed (HTTP " + strconv.Itoa(resp.StatusCode) + ")"}
	}
	limit := maxBytes
	if limit <= 0 {
		limit = MaxDownloadBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", ErrTooLarge
	}
	return body, f.FilePath, nil
}

func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, name)
	if name == "" {
		return "file"
	}
	return name
}
