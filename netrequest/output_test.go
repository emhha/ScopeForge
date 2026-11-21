package netrequest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/emhha/netloom"
)

func TestFormatKeepsBodyLiteralAndMetadataSmall(t *testing.T) {
	body := "<html>\n\"quoted\" \\ path & 中文\n</html>\n"
	out := &Output{URL: "https://example.test/path", Status: 404, Headers: http.Header{"Content-Type": {"text/html"}, "X-Large": {strings.Repeat("x", 30000)}}, Body: body, BodyEncoding: "utf8", BodyBytes: len(body), Complete: true}
	text := Format(out, nil, false)
	head, actual, ok := strings.Cut(text, "\n\n")
	if !ok || actual != body {
		t.Fatalf("response body was altered: %q", text)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(head), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta) != 5 || meta["status_code"] != float64(404) || meta["complete"] != true || strings.Contains(head, "X-Large") {
		t.Fatalf("unexpected default metadata: %s", head)
	}
	if strings.Contains(head, "body_encoding") || strings.Contains(head, "exchange_ids") || strings.Contains(head, "headers") {
		t.Fatalf("empty/default detail leaked into metadata: %s", head)
	}
}

func TestFormatVerboseAndPartialError(t *testing.T) {
	response := &netloom.Response{StatusCode: 200, HTTPVersion: "HTTP/1.1", IP: "203.0.113.7", Header: fhttp.Header{"Content-Type": {"text/plain"}, "X-Repeat": {"one", "two"}}, RequestHeaders: fhttp.Header{"Authorization": {"Bearer secret"}, "X-Test": {"actual"}}}
	out := &Output{URL: "http://example.test/a?b=c", Status: 200, Body: "partial\n", BodyEncoding: "utf8", Hops: []ResponseHop{{URL: "http://example.test/a?b=c", Method: "POST", Response: response}}}
	text := Format(out, errors.Join(errors.New("capture failed"), io.ErrUnexpectedEOF), true)
	for _, want := range []string{`"complete":false`, `"error":`, "unexpected EOF", "capture failed", "> POST /a?b=c HTTP/1.1", "> Host: example.test", "> X-Test: actual", "< HTTP/1.1 200 OK", "< X-Repeat: one", "< X-Repeat: two", "* Connected to 203.0.113.7", "\n\npartial\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "Bearer secret") {
		t.Fatal("verbose exposed request credentials")
	}
	response.Header.Set("X-Large", strings.Repeat("x", 64000))
	text = Format(out, nil, true)
	if len(text) > 18000 || !strings.Contains(text, "verbose output truncated") || !strings.HasSuffix(text, out.Body) {
		t.Fatal("verbose limit discarded the body or exceeded its bound")
	}
}

func TestFormatErrorWithoutResponseAndBinaryPreview(t *testing.T) {
	text := Format(nil, errors.New("target blocked"), true)
	var meta map[string]any
	if err := json.Unmarshal([]byte(text), &meta); err != nil || meta["error"] != "target blocked" || len(meta) != 1 {
		t.Fatalf("metadata=%v err=%v", meta, err)
	}
	text = Format(&Output{Status: 200, Body: "AP8=", BodyEncoding: "base64", PreviewTruncated: true, PreviewError: "decode failed"}, nil, false)
	if !strings.Contains(text, `"body_encoding":"base64"`) || !strings.Contains(text, `"body_preview_truncated":true`) || !strings.Contains(text, `"warning":"decode failed"`) || !strings.HasSuffix(text, "\n\nAP8=") {
		t.Fatalf("invalid binary preview: %s", text)
	}
}
