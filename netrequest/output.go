package netrequest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Format prints a small metadata object followed by the response body, without
// embedding the body in JSON. Verbose adds native per-hop metadata before it.
func Format(out *Output, requestErr error, verbose bool) string {
	meta := struct {
		Passive          []PassiveHop `json:"passive,omitempty"`
		Status           int          `json:"status_code,omitempty"`
		ContentType      string       `json:"content_type,omitempty"`
		FinalURL         string       `json:"final_url,omitempty"`
		BodyBytes        *int         `json:"body_bytes,omitempty"`
		Complete         *bool        `json:"complete,omitempty"`
		ResponseTimeMS   int64        `json:"response_time_ms,omitempty"`
		RedirectCount    int          `json:"redirect_count,omitempty"`
		ExchangeIDs      []string     `json:"exchange_ids,omitempty"`
		BodyEncoding     string       `json:"body_encoding,omitempty"`
		PreviewTruncated bool         `json:"body_preview_truncated,omitempty"`
		Warning          string       `json:"warning,omitempty"`
		Error            string       `json:"error,omitempty"`
	}{}
	if out != nil {
		meta.Passive = out.Passive
		meta.Status = out.Status
		meta.ContentType = out.Headers.Get("Content-Type")
		if len(out.Redirects) > 0 && out.URL != out.Redirects[0].URL {
			meta.FinalURL = out.URL
		}
		if !out.Complete {
			meta.Complete = &out.Complete
		}
		if verbose {
			meta.BodyBytes = &out.BodyBytes
			meta.ResponseTimeMS, meta.RedirectCount = out.ResponseTimeMS, len(out.Redirects)
		}
		meta.ExchangeIDs, meta.PreviewTruncated = out.ExchangeIDs, out.PreviewTruncated
		if out.BodyEncoding == "base64" {
			meta.BodyEncoding = out.BodyEncoding
		}
		meta.Warning = out.PreviewError
	}
	if requestErr != nil {
		meta.Error = conciseError(requestErr)
	}
	var text strings.Builder
	encoder := json.NewEncoder(&text)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(meta)
	if out != nil {
		text.WriteByte('\n')
		if verbose {
			text.WriteString(formatHops(out.Hops))
			text.WriteByte('\n')
		}
		text.WriteString(out.Body)
		return text.String()
	}
	return strings.TrimSuffix(text.String(), "\n")
}

func conciseError(err error) string {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		messages := make([]string, 0, len(joined.Unwrap()))
		for _, cause := range joined.Unwrap() {
			messages = append(messages, conciseError(cause))
		}
		return strings.Join(messages, "; ")
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected EOF; response body is incomplete"
	}
	text := err.Error()
	for _, prefix := range []string{"netloom.Engine.Send: ", "netloom: read response body: ", "netloom: body: "} {
		text = strings.TrimPrefix(text, prefix)
	}
	return text
}

func formatHops(hops []ResponseHop) string {
	const limit = 16384
	var text strings.Builder
	for _, hop := range hops {
		r := hop.Response
		u, err := url.Parse(hop.URL)
		if err != nil || r == nil {
			continue
		}
		fmt.Fprintf(&text, "* %s %s\n", hop.Method, traceValue(hop.URL))
		if r.IP != "" {
			fmt.Fprintf(&text, "* Connected to %s\n", traceValue(r.IP))
		}
		if r.TLS != nil {
			fmt.Fprintf(&text, "* TLS %s; cipher %s\n", traceValue(r.TLS.Version), traceValue(r.TLS.CipherSuite))
		}
		fmt.Fprintf(&text, "> %s %s %s\n", hop.Method, traceValue(u.RequestURI()), r.HTTPVersion)
		if r.RequestHeaders.Get("Host") == "" {
			fmt.Fprintf(&text, "> Host: %s\n", traceValue(u.Host))
		}
		writeHeaders(&text, ">", http.Header(r.RequestHeaders), true)
		text.WriteString(">\n")
		fmt.Fprintf(&text, "< %s %d %s\n", r.HTTPVersion, r.StatusCode, http.StatusText(r.StatusCode))
		writeHeaders(&text, "<", http.Header(r.Header), false)
		text.WriteString("<\n")
		if r.Error != nil {
			fmt.Fprintf(&text, "* %s\n", traceValue(conciseError(r.Error)))
		}
		if text.Len() > limit {
			return text.String()[:limit] + "\n* verbose output truncated at 16 KiB\n"
		}
	}
	return text.String()
}

func writeHeaders(text *strings.Builder, prefix string, headers http.Header, redact bool) {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range headers[key] {
			if text.Len() >= 16384 {
				return
			}
			if redact && (strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Proxy-Authorization")) {
				value = "[redacted]"
			}
			fmt.Fprintf(text, "%s %s: %s\n", prefix, traceValue(key), traceValue(value))
		}
	}
}

func traceValue(value string) string {
	return strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(value)
}
