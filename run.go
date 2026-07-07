package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	backoffMin   = 1 * time.Second
	backoffMax   = 60 * time.Second
	reprobeEvery = 60 * time.Second
)

// cmdRun connects and serves forever, reconnecting with jittered backoff.
func cmdRun() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not paired (run `dtx-agent pair <code>` first): %w", err)
	}
	url := wsURL(cfg.APIURL)
	log.Printf("dtx-agent starting; connecting to %s as agent %s", url, cfg.AgentID)

	backoff := backoffMin
	for {
		connected, err := serve(context.Background(), cfg, url)
		if connected {
			backoff = backoffMin
		}
		if err != nil {
			log.Printf("connection ended: %v", err)
		}
		wait := jitter(backoff)
		log.Printf("reconnecting in %s", wait.Round(time.Millisecond))
		time.Sleep(wait)
		backoff = min(backoff*2, backoffMax)
	}
}

func jitter(d time.Duration) time.Duration {
	// 50–100% of d, so simultaneous agents don't reconnect in lockstep.
	return time.Duration(float64(d) * (0.5 + rand.Float64()*0.5))
}

// agent holds one live connection's state.
type agent struct {
	cfg  Config
	conn *websocket.Conn
	ctx  context.Context

	writeMu sync.Mutex // serializes conn.Write (one writer at a time)

	mu           sync.Mutex
	visionModels []string
	configured   bool
	advertised   bool // whether a capabilities frame has been sent this connection
	lastCap      bool // last advertised llm.vision state
	hintShown    bool
	busy         bool
	jobs         map[string]context.CancelFunc
}

// serve dials, runs the read loop until the connection drops, and reports
// whether the dial succeeded (so the caller can reset backoff).
func serve(parent context.Context, cfg Config, url string) (connected bool, err error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.AgentToken}},
	})
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	log.Printf("connected")

	a := &agent{cfg: cfg, conn: conn, ctx: ctx, jobs: map[string]context.CancelFunc{}}
	go a.reprobeLoop()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return true, err
		}
		a.dispatch(data)
	}
}

func (a *agent) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal frame: %v", err)
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if err := a.conn.Write(a.ctx, websocket.MessageText, b); err != nil {
		log.Printf("write frame: %v", err)
	}
}

func (a *agent) dispatch(data []byte) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("bad frame: %v", err)
		return
	}
	switch routeFrame(f) {
	case kindConfig:
		a.onConfig(f.VisionModels)
	case kindCancel:
		a.cancelJob(f.JobID)
	case kindAssign:
		go a.runJob(f.JobID, f.Payload)
	default:
		log.Printf("ignoring unknown frame type %q", f.Type)
	}
}

func (a *agent) onConfig(visionModels []string) {
	a.mu.Lock()
	a.visionModels = visionModels
	a.configured = true
	a.mu.Unlock()
	a.probeAndAdvertise()
}

func (a *agent) reprobeLoop() {
	t := time.NewTicker(reprobeEvery)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			a.probeAndAdvertise()
		}
	}
}

// probeAndAdvertise probes Ollama and sends a capabilities frame on first
// advertisement or whenever the llm.vision state changes.
func (a *agent) probeAndAdvertise() {
	a.mu.Lock()
	if !a.configured {
		a.mu.Unlock()
		return // wait for the server's config frame before deciding
	}
	models := a.visionModels
	a.mu.Unlock()

	installed, probeErr := probeOllama(a.ctx)
	has := probeErr == nil && hasVisionModel(installed, models)

	a.mu.Lock()
	if probeErr != nil && !a.hintShown {
		printInstallHint(models)
		a.hintShown = true
	}
	if probeErr == nil {
		a.hintShown = false
	}
	changed := !a.advertised || has != a.lastCap
	a.lastCap = has
	a.advertised = true
	a.mu.Unlock()

	if !changed {
		return
	}
	caps := []string{}
	if has {
		caps = []string{"llm.vision"}
	}
	log.Printf("advertising capabilities: %v", caps)
	a.send(map[string]any{"type": "capabilities", "capabilities": caps})
}

func (a *agent) cancelJob(jobID string) {
	a.mu.Lock()
	cancel := a.jobs[jobID]
	a.mu.Unlock()
	if cancel != nil {
		log.Printf("cancelling job %s", jobID)
		cancel()
	}
}

// runJob executes a single LLM_VISION job. One job at a time (plan §2).
func (a *agent) runJob(jobID string, p *VisionPayload) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		a.send(resultFrame(jobID, nil, "agent busy: one job at a time"))
		return
	}
	a.busy = true
	jctx, cancel := context.WithCancel(a.ctx)
	a.jobs[jobID] = cancel
	a.mu.Unlock()

	defer func() {
		cancel()
		a.mu.Lock()
		a.busy = false
		delete(a.jobs, jobID)
		a.mu.Unlock()
	}()

	log.Printf("job %s accepted", jobID)
	a.send(map[string]any{"type": "job.accept", "jobId": jobID})

	// The server names its preferred model blind; substitute what's installed.
	if p != nil {
		a.mu.Lock()
		acceptable := a.visionModels
		a.mu.Unlock()
		if installed, probeErr := probeOllama(jctx); probeErr == nil {
			if picked := pickModel(p.Model, installed, acceptable); picked != p.Model {
				log.Printf("job %s: model %q not installed, running %q", jobID, p.Model, picked)
				p.Model = picked
			}
		}
	}

	result, err := doVision(jctx, p)
	if err != nil {
		log.Printf("job %s failed: %v", jobID, err)
		a.send(resultFrame(jobID, nil, err.Error()))
		return
	}
	log.Printf("job %s done", jobID)
	a.send(resultFrame(jobID, result, ""))
}

// doVision downloads the payload images and runs them through Ollama.
func doVision(ctx context.Context, p *VisionPayload) (json.RawMessage, error) {
	if p == nil {
		return nil, errors.New("assignment had no payload")
	}
	images := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		b, err := download(ctx, f.URL)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", f.URL, err)
		}
		images = append(images, base64.StdEncoding.EncodeToString(b))
	}
	return ollamaChat(ctx, p.Model, p.Prompt, images, p.ResponseSchema)
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}
