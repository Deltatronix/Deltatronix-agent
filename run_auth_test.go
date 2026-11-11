package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type reconnectWaitLog struct{ waits chan time.Duration }

func (w reconnectWaitLog) Write(p []byte) (int, error) {
	if _, after, ok := strings.Cut(string(p), "reconnecting in "); ok {
		if wait, err := time.ParseDuration(strings.TrimSpace(after)); err == nil {
			select {
			case w.waits <- wait:
			default:
			}
		}
	}
	return len(p), nil
}

func TestPostUpgradeUnauthorizedDoesNotResetBackoff(t *testing.T) {
	waits := observeReconnectWaits(t, []websocket.StatusCode{4401, 4401})
	assertReconnectWait(t, waits[0], backoffMin)
	assertReconnectWait(t, waits[1], 2*backoffMin)
}

func TestPolicyRejectionDoesNotResetBackoff(t *testing.T) {
	waits := observeReconnectWaits(t, []websocket.StatusCode{websocket.StatusPolicyViolation, websocket.StatusPolicyViolation})
	assertReconnectWait(t, waits[0], backoffMin)
	assertReconnectWait(t, waits[1], 2*backoffMin)
}

func TestTemporaryAuthFailureRecoversAndTransientDisconnectResetsBackoff(t *testing.T) {
	waits := observeReconnectWaits(t, []websocket.StatusCode{4401, 4401, websocket.StatusGoingAway, websocket.StatusGoingAway})
	assertReconnectWait(t, waits[0], backoffMin)
	assertReconnectWait(t, waits[1], 2*backoffMin)
	assertReconnectWait(t, waits[2], backoffMin)
	assertReconnectWait(t, waits[3], backoffMin)
}

func assertReconnectWait(t *testing.T, wait, backoff time.Duration) {
	t.Helper()
	if wait < backoff/2 || wait > backoff {
		t.Fatalf("retry=%s, want %s..%s", wait, backoff/2, backoff)
	}
}

// Exercise the actual dial/read/retry owner, with no Docker or real credentials.
func observeReconnectWaits(t *testing.T, closes []websocket.StatusCode) []time.Duration {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DTX_AGENT_CONFIG_DIR", t.TempDir())
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer models.Close()
	previousBase := llmBase
	llmBase = models.URL + "/v1"
	defer func() { llmBase = previousBase }()
	var upgrades atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/compute/ws" || r.Header.Get("Authorization") != "Bearer synthetic-revoked-token" {
			t.Error("unexpected paired request")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.CloseNow()
		index := int(upgrades.Add(1)) - 1
		code := closes[min(index, len(closes)-1)]
		if code == websocket.StatusGoingAway {
			// An accepted application session exchanges config/capabilities before its transient close.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := wsjson.Write(ctx, conn, map[string]any{"type": "config", "visionModels": []string{}}); err != nil {
				t.Errorf("config: %v", err)
				return
			}
			var frame map[string]any
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				t.Errorf("accepted session capabilities: %v", err)
				return
			}
			if frame["type"] != "capabilities" {
				t.Errorf("expected capabilities, got %v", frame["type"])
				return
			}
		}
		_ = conn.Close(code, "fixture close")
	}))
	defer server.Close()
	waits := make(chan time.Duration, 8)
	previous := log.Writer()
	log.SetOutput(io.MultiWriter(previous, reconnectWaitLog{waits}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		connectLoop(ctx, Config{APIURL: server.URL, AgentID: "synthetic-revoked-373", AgentToken: "synthetic-revoked-token"})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("connect loop failed to join after cancellation")
		}
		log.SetOutput(previous)
	}()
	var observed []time.Duration
	for range closes {
		select {
		case wait := <-waits:
			observed = append(observed, wait)
		case <-time.After(6 * time.Second):
			t.Fatal("connection did not produce a bounded retry")
		}
	}
	if upgrades.Load() < int32(len(closes)) {
		t.Fatal("test must exercise every real WebSocket upgrade")
	}
	return observed
}

func TestRejectedHTTPHandshakeDoesNotCountAsConnected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		connected, err := serve(context.Background(), Config{AgentToken: "synthetic-token"}, wsURL(server.URL))
		server.Close()
		if connected || err == nil {
			t.Fatalf("HTTP %d unexpectedly counted as a connected session", status)
		}
	}
}

func TestRePairAfterRejectedCredentialRecovers(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DTX_AGENT_CONFIG_DIR", t.TempDir())
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	defer models.Close()
	previousBase := llmBase
	llmBase = models.URL + "/v1"
	defer func() { llmBase = previousBase }()
	accepted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/compute/agents/pair" {
			var input map[string]string
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil || input["code"] != "synthetic-new-code" || r.Header.Get("X-Requested-With") != "dtx-agent" {
				t.Error("unexpected pairing request")
				http.Error(w, "bad pairing", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"agentId":"new-synthetic-pairing","agentToken":"new-synthetic-token"}`)
			return
		}
		if r.URL.Path != "/compute/ws" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.CloseNow()
		if r.Header.Get("Authorization") == "Bearer old-synthetic-token" {
			_ = conn.Close(websocket.StatusCode(4401), "unauthorized")
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-synthetic-token" {
			t.Error("unexpected replacement credential")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := wsjson.Write(ctx, conn, map[string]any{"type": "config", "visionModels": []string{}}); err != nil {
			t.Errorf("config: %v", err)
			return
		}
		var frame map[string]any
		if err := wsjson.Read(ctx, conn, &frame); err != nil || frame["type"] != "capabilities" {
			t.Errorf("replacement did not exchange capabilities: %v", err)
			return
		}
		accepted <- struct{}{}
		_, _, _ = conn.Read(ctx)
	}))
	defer server.Close()
	waits := make(chan time.Duration, 8)
	previousLog := log.Writer()
	log.SetOutput(io.MultiWriter(previousLog, reconnectWaitLog{waits}))
	defer log.SetOutput(previousLog)
	run := func(cfg Config) (context.CancelFunc, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); connectLoop(ctx, cfg) }()
		return cancel, done
	}
	join := func(cancel context.CancelFunc, done <-chan struct{}) {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("old pairing did not stop and join")
		}
	}
	old := Config{APIURL: server.URL, AgentID: "old-synthetic-pairing", AgentToken: "old-synthetic-token"}
	if err := saveConfig(old); err != nil {
		t.Fatal(err)
	}
	cancelOld, oldDone := run(old)
	select {
	case <-waits:
	case <-time.After(3 * time.Second):
		cancelOld()
		t.Fatal("old credential did not reject")
	}
	join(cancelOld, oldDone)
	// The documented CLI repair pairs again, then restarts run with that saved configuration.
	if _, err := pairWithCode(context.Background(), server.URL, "synthetic-new-code"); err != nil {
		t.Fatal(err)
	}
	replacement, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if replacement.AgentID != "new-synthetic-pairing" {
		t.Fatal("pairing was not replaced")
	}
	cancelNew, newDone := run(replacement)
	defer join(cancelNew, newDone)
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("replacement pairing did not recover")
	}
}
