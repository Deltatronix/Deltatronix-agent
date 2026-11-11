package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const (
	backoffMin   = 1 * time.Second
	backoffMax   = 60 * time.Second
	reprobeEvery = 60 * time.Second
)

// cmdRun sets up logging, then either shows the GUI window (default) or runs the
// headless connect loop. headless (via --headless or no display) forces the loop
// with no GUI, keeping server/systemd/Docker deployments working. The GUI handles
// the unpaired case itself (pairing happens in-window); the headless path
// requires an already-paired config.
func cmdRun(headless bool) error {
	settings := loadLLMSettings()
	setupLogging(settings)
	llmBase = settings.BackendURL
	log.Printf("LLM backend: %s", llmBase)

	if !headless && !noDisplay() {
		return runGUI() // opens the window; owns the connect loop; blocks until Quit
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("not paired (run `dtx-agent pair <code>` first): %w", err)
	}
	connectForever(cfg)
	return nil
}

// connectForever runs the connect loop until the process exits — the headless
// entrypoint.
func connectForever(cfg Config) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connectLoop(ctx, cfg)
}

// connectLoop serves and reconnects with jittered backoff, broadcasting status
// as the connection state changes, until ctx is cancelled. The GUI cancels ctx
// (via supervisor.stop) to disconnect live; headless passes a background ctx.
func connectLoop(ctx context.Context, cfg Config) {
	url := wsURL(cfg.APIURL)
	log.Printf("dtx-agent starting; connecting to %s as agent %s", url, cfg.AgentID)
	setStatus(statusConnecting)

	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := serve(ctx, cfg, url)
		// An upgrade can precede application authentication or a policy rejection.
		// Keep retrying transient backend failures, without resetting their backoff.
		closeStatus := websocket.CloseStatus(err)
		if connected && closeStatus != websocket.StatusCode(4401) && closeStatus != websocket.StatusPolicyViolation {
			backoff = backoffMin
		}
		if err != nil {
			log.Printf("connection ended: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		setStatus(statusReconnecting)
		wait := jitter(backoff)
		log.Printf("reconnecting in %s", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// supervisor runs connectLoop under a cancellable context so the GUI can pair
// (start) and disconnect (stop) without restarting the process. Unused in the
// headless build, which calls connectForever directly.
type supervisor struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// start launches the connect loop for cfg if not already running.
func (s *supervisor) start(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	go connectLoop(ctx, cfg)
}

// stop cancels the connect loop and resets status to unpaired.
func (s *supervisor) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	setStatus(statusNotPaired)
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

	jobsWG         sync.WaitGroup // jobs and the reprobe loop; joined on disconnect
	writeMu        sync.Mutex     // serializes conn.Write (one writer at a time)
	settings       LLMSettings
	probeRequested chan struct{}

	mu             sync.Mutex
	visionModels   []string
	configRevision uint64
	configured     bool
	advertised     bool // whether a capabilities frame has been sent this connection
	lastCap        bool // last advertised llm.vision state
	chatReady      bool // last advertised llm.chat state; gates chat admission
	chatProbed     bool // chatReady holds a tool-probe result for the installed model
	hintShown      bool
	busy           bool
	nativeLeases   map[string]nativeLease
	nativeCap      bool
	nativeReady    bool
	jobs           map[string]context.CancelFunc
}

// Retain tombstones across reconnects, scoped to the exact pairing identity.
// Unexpired entries are never evicted to admit new work.
var chatReplay = struct {
	sync.Mutex
	jobs map[chatReplayKey]time.Time
}{jobs: make(map[chatReplayKey]time.Time)}

type chatReplayKey struct {
	profile [32]byte
	jobID   string
}

func claimChatJob(cfg Config, jobID string, busy bool) string {
	key := chatReplayKey{profile: sha256.Sum256([]byte(cfg.APIURL + "\x00" + cfg.AgentID + "\x00" + cfg.AgentToken)), jobID: jobID}
	now := time.Now()
	chatReplay.Lock()
	defer chatReplay.Unlock()
	for key, until := range chatReplay.jobs {
		if !now.Before(until) {
			delete(chatReplay.jobs, key)
		}
	}
	if _, ok := chatReplay.jobs[key]; ok {
		return "CHAT_DUPLICATE_JOB: use a new jobId; this pairing already accepted this job"
	}
	if busy {
		return "agent busy: one job at a time"
	}
	if len(chatReplay.jobs) >= 4096 {
		return "CHAT_REPLAY_LIMIT: replay protection is full; wait for its one-hour retention window"
	}
	chatReplay.jobs[key] = now.Add(time.Hour)
	return ""
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
	setStatus("Connected")

	recoveryErr := reconcileNativeContainers(ctx, cfg.AgentID)
	if recoveryErr != nil {
		log.Printf("native recovery unavailable: %v", recoveryErr)
	}
	a := &agent{nativeReady: recoveryErr == nil, cfg: cfg, conn: conn, ctx: ctx, settings: loadLLMSettings(), jobs: map[string]context.CancelFunc{}, probeRequested: make(chan struct{}, 1)}
	defer func() { cancel(); a.jobsWG.Wait() }()
	a.launchJob(a.reprobeLoop)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return true, err
		}
		a.dispatch(data)
	}
}

func (a *agent) send(v any) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.sendLocked(v)
}

// sendLocked requires writeMu; capability admission shares this write owner.
func (a *agent) sendLocked(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal frame: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	if err := a.conn.Write(ctx, websocket.MessageText, b); err != nil {
		log.Printf("write frame: %v", err)
	}
}

func (a *agent) dispatch(data []byte) {
	// Shape native frames route first, so the chat envelope checks below never
	// refuse them.
	var native struct {
		Type      string          `json:"type"`
		JobType   string          `json:"jobType"`
		JobID     string          `json:"jobId"`
		LeaseID   string          `json:"leaseId"`
		ExpiresAt int64           `json:"expiresAt"`
		Payload   json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(data, &native)
	if native.Type == "job.lease" {
		a.onNativeLease(native.JobID, native.LeaseID, native.ExpiresAt)
		return
	}
	if native.Type == "SHAPE_NATIVE" || (native.Type == "job.assign" && native.JobType == "SHAPE_NATIVE") {
		var p nativePayload
		if err := json.Unmarshal(native.Payload, &p); err != nil {
			a.rejectMalformedPCBJob(native.JobID, native.Payload)
			return
		}
		a.launchJob(func() { a.runNativeJob(native.JobID, &p) })
		return
	}

	var header struct {
		Type    string          `json:"type"`
		JobType json.RawMessage `json:"jobType"`
		JobID   string          `json:"jobId"`
	}
	if json.Unmarshal(data, &header) != nil {
		log.Printf("bad compute frame")
		return
	}
	reject := func(message string) {
		if validChatName(header.JobID, 128) {
			a.send(resultFrame(header.JobID, nil, message))
		}
	}
	var jobType string
	if header.JobType != nil {
		if json.Unmarshal(header.JobType, &jobType) != nil || jobType == "" {
			reject("UNSUPPORTED_JOB_TYPE")
			return
		}
		if checkChatJSON(data) != nil || header.Type != "job.assign" {
			reject("CHAT_INVALID_REQUEST: use a unique-member job.assign envelope")
			return
		}
	}
	if jobType == "LLM_CHAT_TOOLS" || header.Type == "LLM_CHAT_TOOLS" {
		job, err := decodeChatAssignment(data, a.settings)
		if err != nil {
			reject(err.Error())
			return
		}
		a.runJob(job.JobID, nil, &job)
		return
	}
	if jobType != "" && jobType != "LLM_VISION" {
		reject("UNSUPPORTED_JOB_TYPE")
		return
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("bad compute frame")
		return
	}
	switch routeFrame(f) {
	case kindConfig:
		a.onConfig(f.VisionModels)
	case kindCancel:
		a.cancelJob(f.JobID)
	case kindAssign:
		a.runJob(f.JobID, f.Payload, nil)
	default:
		log.Printf("ignoring unknown compute frame")
	}
}

func (a *agent) onConfig(visionModels []string) {
	a.mu.Lock()
	a.visionModels = visionModels
	a.configRevision++
	a.configured = true
	a.mu.Unlock()
	select {
	case a.probeRequested <- struct{}{}:
	default:
	}
}

func (a *agent) reprobeLoop() {
	t := time.NewTicker(reprobeEvery)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
		case <-a.probeRequested:
		}
		// Coalesce config changes; a change during probing stays queued.
		select {
		case <-a.probeRequested:
		default:
		}
		a.probeAndAdvertise()
	}
}

// probeAndAdvertise probes Ollama and sends a capabilities frame on first
// advertisement or whenever a verified vision/chat capability changes.
func (a *agent) probeAndAdvertise() {
	a.mu.Lock()
	if !a.configured || a.busy {
		a.mu.Unlock()
		return // wait for the server's config frame before deciding
	}
	models := a.visionModels
	revision := a.configRevision
	hasChat, chatProbed := a.chatReady, a.chatProbed
	a.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(a.ctx, chatTimeout)
	defer cancel()
	installed, probeErr := probeModels(probeCtx)
	has := probeErr == nil && hasVisionModel(installed, models)
	// The tool probe runs real inference, so it runs once per connection and
	// again only after the selected model leaves and returns to /models.
	// Settings changes restart the agent, which starts a new connection.
	chatInstalled := false
	if localChatSettings(a.settings) {
		listed, err := probeModelsAt(probeCtx, strings.TrimRight(a.settings.BackendURL, "/"), localChatHTTP)
		chatInstalled = err == nil && slices.Contains(listed, a.settings.ChatModel)
		if chatInstalled && !chatProbed {
			hasChat = probeLocalChat(probeCtx, a.settings, listed)
		}
	}
	if !chatInstalled {
		hasChat = false
	}

	a.mu.Lock()
	ready := a.nativeReady
	busy := a.busy // a job may have started since the check above
	a.mu.Unlock()
	if !ready && !busy {
		ready = reconcileNativeContainers(a.ctx, a.cfg.AgentID) == nil
		a.mu.Lock()
		a.nativeReady = ready
		a.mu.Unlock()
	}
	nativeAvailable := ready && probeNativeContainer(a.ctx) == nil

	a.mu.Lock()
	if a.ctx.Err() != nil || a.configRevision != revision {
		a.mu.Unlock()
		return
	}
	if probeErr != nil && !a.hintShown {
		printLLMHint(models)
		a.hintShown = true
	}
	if probeErr == nil {
		a.hintShown = false
	}
	switch {
	case probeErr != nil:
		setStatus(statusNoLLM)
	case has || hasChat:
		setStatus(statusConnected)
	default:
		setStatus(statusNoVisionModel)
	}
	a.mu.Unlock()

	// Publication is admitted only after earlier writes finish. Config updates
	// use mu alone, so the reader and probe cancellation remain responsive.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	if a.ctx.Err() != nil || a.configRevision != revision {
		a.mu.Unlock()
		return
	}
	changed := !a.advertised || has != a.lastCap || hasChat != a.chatReady || nativeAvailable != a.nativeCap
	a.lastCap = has
	a.chatReady = hasChat
	a.chatProbed = chatInstalled
	a.nativeCap = nativeAvailable
	a.advertised = true
	a.mu.Unlock()

	if !changed {
		return
	}
	frame := map[string]any{"type": "capabilities"}
	caps := []string{}
	if has {
		caps = []string{"llm.vision"}
	}
	if hasChat {
		caps = append(caps, "llm.chat")
		// A dispatcher needs the exact model and context cap to build a job.
		frame["chat"] = map[string]any{"model": a.settings.ChatModel, "contextTokens": a.settings.ChatContextTokens}
	}
	if nativeAvailable {
		caps = append(caps, "shape.native.v1", "shape.assembly.v1", "shape.pcb-fit.v1")
	}
	frame["capabilities"] = caps
	log.Printf("advertising capabilities: %v", caps)
	a.sendLocked(frame)
}

func (a *agent) cancelJob(jobID string) {
	a.mu.Lock()
	cancel := a.jobs[jobID]
	if cancel != nil {
		cancel()
	}
	a.mu.Unlock()
	if cancel != nil {
		log.Printf("cancelling assigned job")
	}
}

// runJob admits vision and chat to the same cancellable slot before returning
// to the read loop, so an immediately following job.cancel cannot miss the job.
func (a *agent) runJob(jobID string, p *VisionPayload, chatJob *chatAssignment) {
	a.mu.Lock()
	if chatJob != nil && !a.chatReady {
		a.mu.Unlock()
		a.send(resultFrame(jobID, nil, "CHAT_MODEL_UNAVAILABLE: configure and verify the selected local model first"))
		return
	}
	if chatJob != nil {
		if message := claimChatJob(a.cfg, jobID, a.busy); message != "" {
			a.mu.Unlock()
			a.send(resultFrame(jobID, nil, message))
			return
		}
	}
	if a.busy {
		a.mu.Unlock()
		a.send(resultFrame(jobID, nil, "agent busy: one job at a time"))
		return
	}
	a.busy = true
	jctx, cancel := context.WithCancel(a.ctx)
	a.jobs[jobID] = cancel
	a.mu.Unlock()
	a.launchJob(func() { a.executeJob(jobID, p, chatJob, jctx, cancel) })
}

func (a *agent) executeJob(jobID string, p *VisionPayload, chatJob *chatAssignment, jctx context.Context, cancel context.CancelFunc) {
	var finished sync.Once
	var terminalErr error
	finish := func(doneCtx context.Context) error {
		finished.Do(func() {
			a.mu.Lock()
			terminalErr = doneCtx.Err()
			cancel()
			a.busy = false
			delete(a.jobs, jobID)
			a.mu.Unlock()
		})
		return terminalErr
	}
	defer finish(jctx)

	a.send(map[string]any{"type": "job.accept", "jobId": jobID})
	if chatJob != nil {
		ctx, stop := context.WithTimeout(jctx, min(time.Until(chatJob.ExpiresAt), chatTimeout))
		defer stop()
		installed, err := probeModelsAt(ctx, strings.TrimRight(a.settings.BackendURL, "/"), localChatHTTP)
		var result json.RawMessage
		if err != nil || !slices.Contains(installed, chatJob.Payload.Model) {
			err = errors.New("CHAT_MODEL_UNAVAILABLE: selected model is no longer installed")
		} else {
			result, err = callLocalChat(ctx, a.settings, chatJob.Payload)
		}
		// Completion and job.cancel share the same transition under a.mu.
		terminalErr := finish(ctx)
		if errors.Is(terminalErr, context.Canceled) {
			err = errors.New("CHAT_CANCELLED: request cancelled")
		}
		if errors.Is(terminalErr, context.DeadlineExceeded) {
			err = errors.New("CHAT_EXPIRED: request deadline elapsed")
		}
		if err != nil {
			a.send(resultFrame(jobID, nil, err.Error()))
		} else {
			a.send(resultFrame(jobID, result, ""))
		}
		return
	}
	log.Printf("vision job accepted")

	// The server names its preferred model blind; substitute what's installed.
	if p != nil {
		a.mu.Lock()
		acceptable := a.visionModels
		a.mu.Unlock()
		if installed, probeErr := probeModels(jctx); probeErr == nil {
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
		img := toOllamaImage(b)
		uri := "data:" + http.DetectContentType(img) + ";base64," + base64.StdEncoding.EncodeToString(img)
		images = append(images, uri)
	}
	return chat(ctx, p.Model, p.Prompt, images, p.ResponseSchema)
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

// Register before scheduling so a disconnected reader can safely join every job.
func (a *agent) launchJob(run func()) { a.jobsWG.Add(1); go func() { defer a.jobsWG.Done(); run() }() }
