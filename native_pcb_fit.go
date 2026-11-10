package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

type nativeBoardCapture struct {
	ReferenceID string `json:"referenceId"`
	DesignID    string `json:"designId"`
	BoardID     string `json:"boardId"`
	Capture     struct {
		AssetID          string  `json:"assetId"`
		SourceSnapshotID string  `json:"sourceSnapshotId"`
		SourceHash       string  `json:"sourceHash"`
		RevisionID       *string `json:"revisionId"`
	} `json:"capture"`
	RawSize int64 `json:"rawSize"`
}

var nativeFitPolicy = map[string]float64{"maxBoards": 8, "maxModels": 256, "maxComponents": 256, "maxPrimitives": 2048, "maxKeepouts": 256, "maxChecks": 256}

func validateNativePCB(p *nativePayload) error {
	if p.TargetKind != "shape" || !nativeIDPattern.MatchString(p.RequestID) || !nativeIDPattern.MatchString(p.Lease.ID) || !nativeIDPattern.MatchString(p.DocumentID) || !nativeIDPattern.MatchString(p.SnapshotID) || p.Version < 0 || int64(p.Version) > 9007199254740991 || (p.ConfigurationID != nil && !nativeIDPattern.MatchString(*p.ConfigurationID)) || p.SourceDocuments != nil || p.AssemblyLimits != nil || p.ClosureHash != "" || !nativeHashPattern.MatchString(p.ProofHash) {
		return errors.New("invalid native PCB identity")
	}
	var operation map[string]any
	if json.Unmarshal(p.Operation, &operation) != nil || len(operation) != 1 || operation["kind"] != "pcb-fit" {
		return errors.New("invalid native PCB operation")
	}
	if len(p.FitLimits) != len(nativeFitPolicy) {
		return errors.New("native PCB limits missing or unsupported")
	}
	for key, value := range nativeFitPolicy {
		if p.FitLimits[key] != value {
			return errors.New("unsupported native PCB policy")
		}
	}
	if p.BoardCaptures == nil || len(p.BoardCaptures) > 256 || len(p.Assets) > 256 {
		return errors.New("native PCB proof limit exceeded")
	}
	assets := map[string]int64{}
	for _, asset := range p.Assets {
		assets[asset.Hash] = asset.Size
	}
	seen := map[string]bool{}
	for _, board := range p.BoardCaptures {
		c := board.Capture
		if !nativeIDPattern.MatchString(board.ReferenceID) || !nativeIDPattern.MatchString(board.DesignID) || !nativeIDPattern.MatchString(board.BoardID) || !nativeIDPattern.MatchString(c.SourceSnapshotID) || !nativeHashPattern.MatchString(c.AssetID) || !nativeHashPattern.MatchString(c.SourceHash) || (c.RevisionID != nil && !nativeIDPattern.MatchString(*c.RevisionID)) || seen[board.ReferenceID] || board.RawSize <= 0 || board.RawSize > 4<<20 || assets[c.AssetID] != board.RawSize {
			return errors.New("invalid native PCB capture")
		}
		seen[board.ReferenceID] = true
	}
	digest, err := nativePCBProofHash(p)
	if err != nil {
		return err
	}
	if digest != p.ProofHash {
		return errors.New("native PCB proof hash mismatch")
	}
	return nil
}
func nativePCBProofHash(p *nativePayload) (string, error) {
	boards := append([]nativeBoardCapture{}, p.BoardCaptures...)
	sort.Slice(boards, func(i, j int) bool { return boards[i].ReferenceID < boards[j].ReferenceID })
	assets := append([]nativeFile{}, p.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].Hash < assets[j].Hash })
	metadata := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		metadata = append(metadata, map[string]any{"hash": asset.Hash, "size": asset.Size})
	}
	// Use maps for recursive key ordering, including capture structs.
	boardData, err := json.Marshal(boards)
	if err != nil {
		return "", err
	}
	var boardMetadata []any
	if err = json.Unmarshal(boardData, &boardMetadata); err != nil {
		return "", err
	}
	preimage, err := canonicalNativeJSON(map[string]any{"targetKind": p.TargetKind, "documentId": p.DocumentID, "version": p.Version, "documentHash": p.Document.Hash, "configurationId": p.ConfigurationID, "fitLimits": p.FitLimits, "boardCaptures": boardMetadata, "assets": metadata})
	if err != nil {
		return "", err
	}
	return hashNative(preimage), nil
}

func validateNativePCBSummary(p *nativePayload, summary map[string]any) error {
	fields := []string{"bodyCount", "triangles", "work", "microstamp", "states", "file", "fit", "resultHash", "resultSize"}
	if !nativeExactFields(summary, fields) {
		return errors.New("invalid native PCB summary fields")
	}
	stamp, ok := summary["microstamp"].(string)
	if !ok || len(stamp) == 0 || len(stamp) > 128 {
		return errors.New("invalid native PCB microstamp")
	}
	if summary["file"] != nil {
		return errors.New("PCB verification cannot export files")
	}
	states, ok := summary["states"].(map[string]any)
	if !ok || len(states) != 0 {
		return errors.New("PCB verification cannot return derived feature states")
	}
	fit, ok := summary["fit"].(map[string]any)
	if !ok || !nativeExactFields(fit, []string{"decision", "sourceProofHash", "requirementsHash", "findingsHash", "checkCount", "interferenceCount", "indeterminateCount", "issues", "moreIssues"}) || fit["sourceProofHash"] != p.ProofHash {
		return errors.New("invalid native PCB fit proof")
	}
	for _, key := range []string{"requirementsHash", "findingsHash"} {
		value, ok := fit[key].(string)
		if !ok || !nativeHashPattern.MatchString(value) {
			return errors.New("invalid native PCB fit digest")
		}
	}
	for _, key := range []string{"checkCount", "interferenceCount", "indeterminateCount", "moreIssues"} {
		if !nativeSummaryCount(fit[key], 256) {
			return errors.New("invalid native PCB fit count")
		}
	}
	checks := fit["checkCount"].(float64)
	interferences := fit["interferenceCount"].(float64)
	unknown := fit["indeterminateCount"].(float64)
	issues, ok := fit["issues"].([]any)
	if !ok || len(issues) > 64 || interferences > checks {
		return errors.New("invalid native PCB issues")
	}
	more := fit["moreIssues"].(float64)
	if more > 0 && len(issues) != 64 {
		return errors.New("invalid native PCB issue truncation")
	}
	for _, raw := range issues {
		issue, ok := raw.(map[string]any)
		if !ok || !nativeExactFields(issue, []string{"featureId", "referenceId", "refdes", "code"}) {
			return errors.New("invalid native PCB issue fields")
		}
		for _, key := range []string{"featureId", "referenceId", "refdes"} {
			if issue[key] != nil {
				value, ok := issue[key].(string)
				if !ok || len(value) == 0 || len(value) > 128 {
					return errors.New("invalid native PCB issue identity")
				}
			}
		}
		switch issue["code"] {
		case "interference", "unknown-model", "keepout-unavailable", "selection-unresolved", "checks-disabled", "no-checks", "measurement-failed", "feature-failed", "opening-missed":
		default:
			return errors.New("unknown native PCB issue code")
		}
	}
	switch fit["decision"] {
	case "clear":
		if checks == 0 || interferences != 0 || unknown != 0 || len(issues) != 0 || more != 0 {
			return errors.New("native PCB clear claim lacks complete coverage")
		}
	case "interference":
		if interferences == 0 {
			return errors.New("native PCB interference claim lacks measurement")
		}
	case "indeterminate":
		if interferences != 0 || unknown == 0 {
			return errors.New("invalid native PCB indeterminate claim")
		}
	default:
		return errors.New("unknown native PCB fit decision")
	}
	return nil
}
func nativeExactFields(object map[string]any, fields []string) bool {
	if len(object) != len(fields) {
		return false
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return false
		}
	}
	return true
}

// Older protocols keep their established permissive assignment decoding.
func (p *nativePayload) UnmarshalJSON(data []byte) error {
	type payload nativePayload
	var version struct {
		ContractVersion int `json:"contractVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return err
	}
	if version.ContractVersion != 3 {
		return json.Unmarshal(data, (*payload)(p))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*payload)(p)); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("invalid native PCB trailing JSON")
	}
	return nil
}

func (a *agent) rejectMalformedPCBJob(jobID string, payload []byte) {
	var identity struct {
		ContractVersion int `json:"contractVersion"`
		Lease           struct {
			ID string `json:"id"`
		} `json:"lease"`
	}
	var version struct {
		ContractVersion int `json:"contractVersion"`
	}
	if json.Unmarshal(payload, &version) != nil || version.ContractVersion != 3 {
		return
	}
	if json.Unmarshal(payload, &identity) != nil {
		a.conn.CloseNow()
		return
	}
	// A malformed or stale frame must never acknowledge an active job's lease.
	a.mu.Lock()
	busy := a.busy
	_, owned := a.nativeLeases[jobID]
	a.mu.Unlock()
	if !nativeIDPattern.MatchString(jobID) || !nativeIDPattern.MatchString(identity.Lease.ID) || busy || owned {
		a.conn.CloseNow()
		return
	}
	a.send(map[string]any{"type": "job.result", "jobId": jobID, "leaseId": identity.Lease.ID, "ok": false, "error": "Malformed PCB fit assignment"})
}
