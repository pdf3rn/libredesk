// Command launcher starts the libredesk Docker stack, waits until the app answers and opens it
// in a browser. With --screenshot <file.png> it saves a picture of the app instead and exits.
// The server itself lives in ./cmd.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fasthttp/websocket"
)

const appURL = "http://localhost:9000"

func main() {
	shot := flag.String("screenshot", "", "save a PNG of the app to this file and exit")
	flag.Parse()

	if err := startStack(); err != nil {
		fail("starting docker stack: %v", err)
	}
	if err := waitHealthy(3 * time.Minute); err != nil {
		fail("%v", err)
	}

	if *shot != "" {
		if err := screenshot(*shot); err != nil {
			fail("screenshot: %v", err)
		}
		fmt.Println("saved", *shot)
		return
	}

	fmt.Println("libredesk is running at", appURL)
	if err := exec.Command("xdg-open", appURL).Start(); err != nil {
		fmt.Println("open", appURL, "in your browser")
	}
}

// startStack brings the compose stack up: from the host-built binary when present, else built in Docker.
func startStack() error {
	args := []string{"compose", "-f", "docker-compose.build.yml", "up", "-d", "--build"}
	if _, err := os.Stat(filepath.Join("bin", "libredesk")); err == nil {
		args = []string{"compose", "-f", "docker-compose.prebuilt.yml", "up", "-d"}
	}
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func waitHealthy(timeout time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(appURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("libredesk did not answer on " + appURL + "/health in time")
}

// screenshot drives a headless Chromium-based browser over DevTools: it signs in as the System user
// (password from .env) when it can, opens the new-inbox page on the Telegram form and saves a PNG.
func screenshot(out string) error {
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	browser := ""
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave-browser", "microsoft-edge"} {
		if p, err := exec.LookPath(name); err == nil {
			browser = p
			break
		}
	}
	if browser == "" {
		return errors.New("no Chromium-based browser found")
	}
	profile, err := os.MkdirTemp("", "libredesk-shot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)

	cmd := exec.Command(browser, "--headless=new", "--disable-gpu", "--no-first-run", "--hide-scrollbars",
		"--remote-debugging-port=0", "--user-data-dir="+profile, "--window-size=1440,900", "about:blank")
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	cdp, err := connectCDP(profile, 30*time.Second)
	if err != nil {
		return err
	}
	defer cdp.conn.Close()

	if _, err := cdp.call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1440, "height": 900, "deviceScaleFactor": 1, "mobile": false}); err != nil {
		return err
	}
	if _, err := cdp.call("Page.navigate", map[string]any{"url": appURL + "/"}); err != nil {
		return err
	}
	time.Sleep(4 * time.Second)

	if pass := envValue(".env", "LIBREDESK_SYSTEM_USER_PASSWORD"); pass != "" {
		pw, _ := json.Marshal(pass)
		login := `(async () => {
			const m = document.cookie.match(/csrf_token=([^;]+)/);
			const r = await fetch('/api/v1/auth/login', {method: 'POST', credentials: 'include',
				headers: {'Content-Type': 'application/json', 'X-CSRFTOKEN': m ? m[1] : ''},
				body: JSON.stringify({email: 'System', password: ` + string(pw) + `})});
			return r.status;
		})()`
		res, err := cdp.call("Runtime.evaluate", map[string]any{"expression": login, "awaitPromise": true, "returnByValue": true})
		if err == nil && strings.Contains(string(res), `"value":200`) {
			if _, err := cdp.call("Page.navigate", map[string]any{"url": appURL + "/admin/inboxes/new"}); err != nil {
				return err
			}
			time.Sleep(4 * time.Second)
			click := `(() => { const el = [...document.querySelectorAll('h1,h2,h3,h4,p,span,div')]
				.find(e => e.children.length === 0 && e.textContent.trim() === 'Telegram');
				if (el) el.click(); return !!el; })()`
			_, _ = cdp.call("Runtime.evaluate", map[string]any{"expression": click, "returnByValue": true})
			time.Sleep(2 * time.Second)
		}
	}

	res, err := cdp.call("Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return err
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &shot); err != nil {
		return err
	}
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil || len(png) == 0 {
		return errors.New("browser returned no picture")
	}
	return os.WriteFile(abs, png, 0o644)
}

type cdpClient struct {
	conn *websocket.Conn
	next int
}

// connectCDP waits for the browser's DevTools port and attaches to its first page.
func connectCDP(profile string, timeout time.Duration) (*cdpClient, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err == nil {
			port := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
			resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
			if err == nil {
				var targets []struct {
					Type string `json:"type"`
					WS   string `json:"webSocketDebuggerUrl"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&targets)
				resp.Body.Close()
				for _, t := range targets {
					if t.Type == "page" && t.WS != "" {
						conn, _, err := websocket.DefaultDialer.Dial(t.WS, nil)
						if err != nil {
							return nil, err
						}
						return &cdpClient{conn: conn}, nil
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, errors.New("browser DevTools did not start")
}

// call sends one DevTools command and returns its result, skipping events.
func (c *cdpClient) call(method string, params map[string]any) (json.RawMessage, error) {
	c.next++
	id := c.next
	if err := c.conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		var msg struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := c.conn.ReadJSON(&msg); err != nil {
			return nil, err
		}
		if msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
		}
		return msg.Result, nil
	}
}

// envValue reads KEY=value from a dotenv file.
func envValue(file, key string) string {
	raw, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	return ""
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "libredesk launcher: "+format+"\n", a...)
	os.Exit(1)
}
