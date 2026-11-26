package setupui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigsasha/cloudedits/internal/config"
)

const testKey = "tskey-auth-kSETUPTEST-0123456789abcdef"

func ownPost(t *testing.T, ts *httptest.Server, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func getState(t *testing.T, ts *httptest.Server) (map[string]any, string) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("state: %v: %s", err, b)
	}
	return m, string(b)
}

func waitPhase(t *testing.T, ts *httptest.Server, want Phase) (map[string]any, []string) {
	t.Helper()
	var bodies []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, raw := getState(t, ts)
		bodies = append(bodies, raw)
		c := m["connect"].(map[string]any)
		if c["phase"] == string(want) && c["running"] == false {
			return c, bodies
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connect never reached %s: %s", want, bodies[len(bodies)-1])
	return nil, nil
}

func withFolder(t *testing.T) *config.Config {
	cfg := config.Default(t.TempDir())
	if _, err := cfg.AddFolder(t.TempDir(), "Photos"); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The wizard runs the connect job in the background, reports each phase,
// hands the pasted key to the provider unchanged, and never echoes it.
func TestConnectRunsInTheBackgroundAndNeverEchoesTheKey(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var gotKey string
	st := &stubState{cfg: withFolder(t), connectFn: func(ctx context.Context, key string, phase func(Phase)) (ConnectResult, error) {
		mu.Lock()
		gotKey = key
		mu.Unlock()
		phase(PhaseJoining)
		<-release
		phase(PhaseStarting)
		return ConnectResult{ActionRequired: "Enable HTTPS"}, nil
	}}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()

	code, body := ownPost(t, ts, "/api/connect", `{"authKey":"  `+testKey+`\n"}`)
	if code != http.StatusAccepted {
		t.Fatalf("connect: %d %s", code, body)
	}
	if code, _ := ownPost(t, ts, "/api/connect", `{"authKey":"`+testKey+`"}`); code != http.StatusConflict {
		t.Fatalf("a second connect while one runs: %d, want 409", code)
	}
	m, during := getState(t, ts)
	if c := m["connect"].(map[string]any); c["phase"] != string(PhaseJoining) || c["running"] != true {
		t.Fatalf("while joining: %v", c)
	}
	close(release)
	c, bodies := waitPhase(t, ts, PhaseDone)
	if c["actionRequired"] != "Enable HTTPS" {
		t.Fatalf("done state: %v", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotKey != testKey {
		t.Fatalf("provider got key %q", gotKey)
	}
	for _, b := range append(bodies, body, during) {
		if strings.Contains(b, testKey) || strings.Contains(b, "kSETUPTEST") {
			t.Fatalf("a wizard response contains the auth key: %s", b)
		}
	}
}

func TestConnectErrorIsShownWithTheKeyRedacted(t *testing.T) {
	st := &stubState{cfg: withFolder(t), connectFn: func(ctx context.Context, key string, phase func(Phase)) (ConnectResult, error) {
		return ConnectResult{}, errors.New("tsnet up: backend: invalid authkey " + key)
	}}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()
	if code, body := ownPost(t, ts, "/api/connect", `{"authKey":"`+testKey+`"}`); code != http.StatusAccepted {
		t.Fatalf("connect: %d %s", code, body)
	}
	c, _ := waitPhase(t, ts, PhaseFailed)
	msg, _ := c["error"].(string)
	if !strings.Contains(msg, "invalid authkey") || strings.Contains(msg, testKey) {
		t.Fatalf("error = %q", msg)
	}
}

func TestConnectValidatesBeforeStarting(t *testing.T) {
	started := false
	connect := func(context.Context, string, func(Phase)) (ConnectResult, error) {
		started = true
		return ConnectResult{}, nil
	}
	noFolders := &stubState{cfg: config.Default(t.TempDir()), connectFn: connect}
	withF := &stubState{cfg: withFolder(t), connectFn: connect}
	for name, tc := range map[string]struct {
		st   *stubState
		body string
		want string
	}{
		"no folders":        {noFolders, `{"authKey":"` + testKey + `"}`, "Add a photo folder first."},
		"not a key":         {withF, `{"authKey":"hello"}`, "Tailscale auth keys start with tskey-."},
		"empty, unenrolled": {withF, `{"authKey":""}`, "Paste a Tailscale auth key."},
		"oversized":         {withF, `{"authKey":"tskey-` + strings.Repeat("a", 600) + `"}`, "That is not a Tailscale auth key."},
	} {
		ts := httptest.NewServer(Handler(tc.st))
		code, body := ownPost(t, ts, "/api/connect", tc.body)
		ts.Close()
		if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want 400 %q", name, code, body, tc.want)
		}
	}
	if started {
		t.Fatal("an invalid request started the connect job")
	}

	// Once enrolled, connecting without a key only (re)starts the service.
	enrolled := &stubState{cfg: withFolder(t), server: ServerState{Enrolled: true}, connectFn: func(_ context.Context, key string, _ func(Phase)) (ConnectResult, error) {
		if key != "" {
			return ConnectResult{}, errors.New("unexpected key")
		}
		return ConnectResult{}, nil
	}}
	ts := httptest.NewServer(Handler(enrolled))
	defer ts.Close()
	if code, body := ownPost(t, ts, "/api/connect", `{"authKey":""}`); code != http.StatusAccepted {
		t.Fatalf("restart without key: %d %s", code, body)
	}
	waitPhase(t, ts, PhaseDone)
}

func TestConfigChangesNotifyTheProvider(t *testing.T) {
	st := &stubState{cfg: config.Default(t.TempDir())}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()
	if code, body := ownPost(t, ts, "/api/folders", `{"path":"`+t.TempDir()+`"}`); code != http.StatusOK {
		t.Fatalf("add folder: %d %s", code, body)
	}
	if code, body := ownPost(t, ts, "/api/config", `{"hostname":"photos"}`); code != http.StatusOK {
		t.Fatalf("config: %d %s", code, body)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.changed != 2 {
		t.Fatalf("ConfigChanged called %d times, want 2", st.changed)
	}
}
