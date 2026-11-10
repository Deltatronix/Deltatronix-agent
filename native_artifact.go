package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const nativeReleasePublicKey = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAU4pbMm0KUIuKkVLCERYuit0E2SfeDP+vou0VJNkWfFY=\n-----END PUBLIC KEY-----\n"
const nativeReleaseKeyID = "shape-native-release-2026-11"

func hashNative(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

// encoding/json sorts map keys recursively; disable its HTML escaping to match
// the release signer's compact UTF-8 JSON representation.
func canonicalNativeJSON(v any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func verifyNativeManifest(data []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	signatureObject, ok := m["signature"].(map[string]any)
	if !ok || signatureObject["keyId"] != nativeReleaseKeyID || signatureObject["algorithm"] != "ed25519" {
		return nil, errors.New("unknown native release key or signature")
	}
	signature, ok := signatureObject["valueBase64"].(string)
	if !ok {
		return nil, errors.New("missing native release signature")
	}
	delete(m, "signature")
	signed, err := canonicalNativeJSON(m)
	if err != nil {
		return nil, err
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode([]byte(nativeReleasePublicKey))
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key.(ed25519.PublicKey), signed, sig) {
		return nil, errors.New("invalid native runtime signature")
	}
	return m, nil
}

func nativeFetch(ctx context.Context, target, token string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("too many redirects")
		}
		req.Header.Del("Authorization")
		if req.URL.Scheme != "https" {
			return errors.New("insecure artifact redirect")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("native download status %d", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return nil, errors.New("native download exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("native download exceeds limit")
	}
	return data, nil
}

func nativeAPIURL(cfg Config, path string) string { return strings.TrimRight(cfg.APIURL, "/") + path }

func unpackNativeArchive(data []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, 256<<20))
	total := int64(0)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\\") || filepath.Clean(name) != name || strings.HasPrefix(name, "../") {
			return errors.New("unsafe native archive path")
		}
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(filepath.Join(dest, name), 0755); err != nil {
				return err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return errors.New("native archive contains nonregular entry")
		}
		total += h.Size
		if h.Size < 0 || total > 256<<20 {
			return errors.New("native archive exceeds limit")
		}
		if name != "shape-native" && name != "replicad_single.wasm" && !strings.HasPrefix(name, "LICENSES/") && name != "NOTICE" && name != "provenance.json" {
			return fmt.Errorf("unexpected native archive entry %s", name)
		}
		path := filepath.Join(dest, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if name == "shape-native" {
			mode = 0755
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(f, tr, h.Size)
		closeErr := f.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	for _, name := range []string{"shape-native", "replicad_single.wasm"} {
		info, err := os.Stat(filepath.Join(dest, name))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("native archive missing %s", name)
		}
	}
	return nil
}
