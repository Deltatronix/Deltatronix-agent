package main

import (
	"encoding/json"
	"strings"
)

// Frame is the union of every server→agent frame we care about. Every frame is
// `{ "type": "<name>", ...fields }`. `job.assign` nominally carries both an
// envelope `type` ("job.assign") and a job `type` field — a duplicate JSON key.
// Go's decoder keeps the last value, so on such a frame `Type` may end up as the
// job type rather than "job.assign"; routeFrame handles that by falling back to
// payload-presence. See README "Contract notes".
type Frame struct {
	Type         string         `json:"type"`
	VisionModels []string       `json:"visionModels"`
	JobID        string         `json:"jobId"`
	Payload      *VisionPayload `json:"payload"`
}

// VisionPayload is the LLM_VISION job.assign payload (plan §1 Frames list).
type VisionPayload struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Files          []VisionFile    `json:"files"`
	ResponseSchema json.RawMessage `json:"responseSchema"`
}

type VisionFile struct {
	URL       string `json:"url"`
	MediaType string `json:"mediaType"`
}

// frameKind classifies a decoded frame into one of the actions the agent takes.
type frameKind int

const (
	kindUnknown frameKind = iota
	kindConfig
	kindCancel
	kindAssign
)

// routeFrame decides what a frame means, robust to the job.assign duplicate-key
// collision: a config or job.cancel frame never carries a payload, so any frame
// that does — regardless of what its `type` decoded to — is a job assignment.
func routeFrame(f Frame) frameKind {
	switch f.Type {
	case "config":
		return kindConfig
	case "job.cancel":
		return kindCancel
	case "job.assign":
		return kindAssign
	}
	if f.Payload != nil {
		return kindAssign
	}
	return kindUnknown
}

// resultFrame builds a job.result frame. A non-empty errStr means failure.
func resultFrame(jobID string, result json.RawMessage, errStr string) map[string]any {
	if errStr != "" {
		return map[string]any{"type": "job.result", "jobId": jobID, "ok": false, "error": errStr}
	}
	return map[string]any{"type": "job.result", "jobId": jobID, "ok": true, "result": result}
}

// wsURL derives the compute WebSocket URL from the REST API base.
func wsURL(apiURL string) string {
	u := strings.TrimRight(apiURL, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u + "/compute/ws"
}
