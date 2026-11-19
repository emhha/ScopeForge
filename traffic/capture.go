package traffic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Capture is the application's persisted exchange metadata, independent of the transport.
type Capture struct {
	URL, Method, Proto, Host        string
	RequestHeaders, ResponseHeaders http.Header
	RequestBody, ResponseBody       []byte
	StatusCode                      int
	Complete                        bool
}

// CaptureWriter streams response evidence to disk; only the bounded search prefix stays in memory.
type CaptureWriter struct {
	traffic *Traffic
	capture Capture
	file    *os.File
	hash    hash.Hash
	prefix  []byte
	size    int64
	done    bool
}

func (t *Traffic) NewCapture(c Capture) (*CaptureWriter, error) {
	dir := filepath.Join(t.dir, "_pending")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "response-")
	if err != nil {
		return nil, err
	}
	c.RequestHeaders = c.RequestHeaders.Clone()
	c.ResponseHeaders = c.ResponseHeaders.Clone()
	return &CaptureWriter{traffic: t, capture: c, file: f, hash: sha256.New()}, nil
}
func (w *CaptureWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, os.ErrClosed
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.size += int64(n)
		keep := min(n, maxIndexBody-len(w.prefix))
		if keep > 0 {
			w.prefix = append(w.prefix, p[:keep]...)
		}
	}
	return n, err
}
func (w *CaptureWriter) Abort() {
	if !w.done {
		w.done = true
		_ = w.file.Close()
		_ = os.Remove(w.file.Name())
	}
}
func (w *CaptureWriter) Finish(complete bool) (string, error) {
	if w.done {
		return "", os.ErrClosed
	}
	w.done = true
	defer os.Remove(w.file.Name())
	if err := w.file.Close(); err != nil {
		return "", err
	}
	t := w.traffic
	// Keep blob publication and its index transaction atomic with host deletion.
	t.wmu.Lock()
	defer t.wmu.Unlock()
	body := storedBody{}
	if !isBinaryBody(w.capture.ResponseHeaders.Get("Content-Type"), w.prefix) {
		body.index = string(w.prefix)
	}
	if w.size <= maxInlineBody {
		body.inline = w.prefix
	} else {
		body.hash = hex.EncodeToString(w.hash.Sum(nil))
		if isBinaryBody(w.capture.ResponseHeaders.Get("Content-Type"), w.prefix) {
			body.inline = []byte(binaryTag(w.capture.ResponseHeaders.Get("Content-Type"), w.prefix))
		} else {
			body.inline = []byte(truncateUTF8(w.prefix, blobPreview))
		}
		path := filepath.Join(t.dir, "_blobs", "sha256", body.hash[:2], body.hash+".bin")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return "", err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.Rename(w.file.Name(), path); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
	}
	w.capture.Complete = complete
	return t.recordCapture(w.capture, t.spill(w.capture.RequestBody, w.capture.RequestHeaders.Get("Content-Type")), body, w.size)
}
func (t *Traffic) Record(c Capture) (string, error) {
	w, err := t.NewCapture(c)
	if err != nil {
		return "", err
	}
	defer w.Abort()
	if _, err = io.Copy(w, bytes.NewReader(c.ResponseBody)); err != nil {
		return "", err
	}
	return w.Finish(c.Complete)
}
func (t *Traffic) CaptureComplete(id string) (*bool, error) {
	var complete *bool
	err := t.db.QueryRow("SELECT capture_complete FROM exchanges WHERE id=?", id).Scan(&complete)
	return complete, err
}
