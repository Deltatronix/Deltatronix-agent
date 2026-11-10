package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
)

type nativeSourceDocument struct {
	Kind            string     `json:"kind"`
	ID              string     `json:"id"`
	Version         int        `json:"version"`
	SnapshotID      string     `json:"snapshotId"`
	ConfigurationID *string    `json:"configurationId"`
	Document        nativeFile `json:"document"`
}

var nativeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var nativeAssemblyPolicy = map[string]float64{"maxSourceDocuments": 64, "maxDepth": 16, "maxOccurrences": 4096, "maxMates": 512, "maxPatterns": 256}

func nativeSourceMetadata(source nativeSourceDocument) map[string]any {
	return map[string]any{"kind": source.Kind, "id": source.ID, "version": source.Version, "snapshotId": source.SnapshotID, "hash": source.Document.Hash, "size": source.Document.Size, "configurationId": source.ConfigurationID}
}

func validateNativeAssembly(p *nativePayload, total int64) error {
	if p.TargetKind != "assembly" || p.ConfigurationID != nil || !nativeIDPattern.MatchString(p.DocumentID) || !nativeIDPattern.MatchString(p.SnapshotID) || p.Version < 0 || p.SourceDocuments == nil || len(p.SourceDocuments) > 64 || !nativeHashPattern.MatchString(p.ClosureHash) {
		return errors.New("invalid native assembly identity")
	}
	var operation map[string]any
	if json.Unmarshal(p.Operation, &operation) != nil || len(operation) != 1 || operation["kind"] != "assembly-verify" {
		return errors.New("invalid native assembly operation")
	}
	if len(p.AssemblyLimits) != len(nativeAssemblyPolicy) {
		return errors.New("native assembly limits missing or unsupported")
	}
	for key, value := range nativeAssemblyPolicy {
		if p.AssemblyLimits[key] != value {
			return fmt.Errorf("unsupported native assembly policy %s", key)
		}
	}
	identities := map[string]bool{}
	files := map[string]int64{}
	for _, source := range p.SourceDocuments {
		if (source.Kind != "shape" && source.Kind != "assembly") || !nativeIDPattern.MatchString(source.ID) || !nativeIDPattern.MatchString(source.SnapshotID) || source.Version < 0 || !nativeHashPattern.MatchString(source.Document.Hash) || source.Document.Size <= 0 || source.Document.Size > 4<<20 || (source.ConfigurationID != nil && !nativeIDPattern.MatchString(*source.ConfigurationID)) {
			return errors.New("invalid native assembly source")
		}
		key, _ := canonicalNativeJSON([]any{source.Kind, source.ID, source.Version, source.ConfigurationID})
		if identities[string(key)] {
			return errors.New("duplicate native assembly source")
		}
		identities[string(key)] = true
		if size, exists := files[source.Document.Hash]; exists {
			if size != source.Document.Size {
				return errors.New("native source hash has conflicting sizes")
			}
		} else {
			files[source.Document.Hash] = source.Document.Size
			total += source.Document.Size
		}
		if total > 64<<20 {
			return errors.New("native assembly input exceeds limit")
		}
	}
	digest, err := nativeAssemblyClosureHash(p)
	if err != nil {
		return err
	}
	if digest != p.ClosureHash {
		return errors.New("native assembly closure hash mismatch")
	}
	return nil
}

func nativeAssemblyClosureHash(p *nativePayload) (string, error) {
	sources := append([]nativeSourceDocument(nil), p.SourceDocuments...)
	sort.Slice(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.ConfigurationID == nil {
			return b.ConfigurationID != nil
		}
		if b.ConfigurationID == nil {
			return false
		}
		return *a.ConfigurationID < *b.ConfigurationID
	})
	sourceMetadata := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		sourceMetadata = append(sourceMetadata, nativeSourceMetadata(source))
	}
	assets := append([]nativeFile(nil), p.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].Hash < assets[j].Hash })
	assetMetadata := make([]map[string]any, 0, len(assets))
	for _, asset := range assets {
		assetMetadata = append(assetMetadata, map[string]any{"hash": asset.Hash, "size": asset.Size})
	}
	preimage, err := canonicalNativeJSON(map[string]any{"targetKind": p.TargetKind, "documentId": p.DocumentID, "version": p.Version, "documentHash": p.Document.Hash, "assemblyLimits": p.AssemblyLimits, "sourceDocuments": sourceMetadata, "assets": assetMetadata})
	if err != nil {
		return "", err
	}
	return hashNative(preimage), nil
}

func stageNativeSources(ctx context.Context, p *nativePayload, input string) error {
	staged := map[string]bool{}
	for _, source := range p.SourceDocuments {
		if staged[source.Document.Hash] {
			continue
		}
		if err := fetchNativeInput(ctx, source.Document, filepath.Join(input, "sources", source.Document.Hash+".json")); err != nil {
			return err
		}
		staged[source.Document.Hash] = true
	}
	return nil
}

func validateNativeAssemblySummary(p *nativePayload, summary map[string]any) error {
	fields := map[string]bool{"bodyCount": true, "triangles": true, "work": true, "microstamp": true, "states": true, "file": true, "assembly": true, "resultHash": true, "resultSize": true}
	if len(summary) != len(fields) {
		return errors.New("invalid native assembly summary fields")
	}
	for key := range summary {
		if !fields[key] {
			return errors.New("unexpected derived assembly summary field")
		}
	}
	states, ok := summary["states"].(map[string]any)
	if !ok || len(states) != 0 || summary["file"] != nil {
		return errors.New("assembly verification cannot return feature states or export files")
	}
	assembly, ok := summary["assembly"].(map[string]any)
	if !ok || len(assembly) != 5 || assembly["closureHash"] != p.ClosureHash || !nativeSummaryCount(assembly["occurrenceCount"], p.AssemblyLimits["maxOccurrences"]) || !nativeSummaryCount(assembly["mateCount"], p.AssemblyLimits["maxMates"]) {
		return errors.New("invalid native assembly verification summary")
	}
	for _, key := range []string{"bomHash", "verificationHash"} {
		value, ok := assembly[key].(string)
		if !ok || !nativeHashPattern.MatchString(value) {
			return errors.New("invalid native assembly verification digest")
		}
	}
	return nil
}
