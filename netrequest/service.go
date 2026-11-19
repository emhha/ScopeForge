// Package netrequest wires native netloom requests to the existing traffic store.
package netrequest

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/emhha/netloom"
	sdkhttp "github.com/emhha/netloom/nethttp"
	"github.com/emhha/netloom/passive"
	"github.com/emhha/scopeforge/traffic"
)

type Service struct {
	engine *netloom.Engine
	proxy  func() string
}

func New(proxy func() string) (*Service, error) {
	engine, err := netloom.New(netloom.Config{Threads: 16, MaxConcurrentPerHost: 4, Retries: -1, Timeout: 30 * time.Second, MaxBodySize: 8 << 20, ConnectionPool: &netloom.ConnectionPoolConfig{MaxClients: 16, IdleTimeout: 30 * time.Second}})
	if err != nil {
		return nil, err
	}
	return &Service{engine: engine, proxy: proxy}, nil
}
func (s *Service) Close() error { return s.engine.Close() }

type Output struct {
	URL              string                `json:"url"`
	Status           int                   `json:"status_code"`
	Headers          http.Header           `json:"headers"`
	Body             string                `json:"body"`
	BodyEncoding     string                `json:"body_encoding"`
	BodyBytes        int                   `json:"body_bytes"`
	PreviewError     string                `json:"body_preview_error,omitempty"`
	PreviewTruncated bool                  `json:"body_preview_truncated"`
	Complete         bool                  `json:"complete"`
	ExchangeIDs      []string              `json:"exchange_ids"`
	Redirects        []netloom.RedirectHop `json:"redirects"`
	ResponseTimeMS   int64                 `json:"response_time_ms"`
}

func (s *Service) Do(ctx context.Context, in sdkhttp.RequestInput, store *traffic.Traffic, validate func(context.Context, string) error) (*Output, error) {
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > 120 {
		return nil, errors.New("timeout_seconds must be 0–120")
	}
	req, err := sdkhttp.BuildRequest(in)
	if err != nil {
		return nil, err
	}
	timeout := 30 * time.Second
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if s.proxy != nil {
		req.Proxy = s.proxy()
	}
	sender := &captureSender{engine: s.engine, store: store, ids: []string{}}
	exchange, err := netloom.Do(ctx, sender, req, netloom.RedirectPolicy{Follow: in.FollowRedirects, MaxHops: in.MaxRedirects, ValidateURL: validate})
	if err != nil {
		return nil, err
	}
	r := exchange.Response
	preview, previewTruncated, decodeErr := passive.DecodeBody(r.Body, r.Header.Get("Content-Encoding"), 16384)
	previewError := ""
	if decodeErr != nil {
		preview = r.Body
		if len(preview) > 16384 {
			preview = preview[:16384]
		}
		previewError = decodeErr.Error()
		previewTruncated = true
	}
	encoding, body := "utf8", string(preview)
	if !utf8.Valid(preview) || decodeErr != nil {
		encoding, body = "base64", base64.StdEncoding.EncodeToString(preview)
	}
	return &Output{URL: exchange.Request.URL, Status: r.StatusCode, Headers: http.Header(r.Header), Body: body, BodyEncoding: encoding, BodyBytes: len(r.Body), PreviewTruncated: previewTruncated, PreviewError: previewError, Complete: !r.BodyTruncated, ExchangeIDs: sender.ids, Redirects: exchange.Hops, ResponseTimeMS: r.ResponseTime.Milliseconds()}, nil
}

// Native Do owns redirects and sensitive-header policy; this sender only persists each exchange.
type captureSender struct {
	engine *netloom.Engine
	store  *traffic.Traffic
	ids    []string
}

func (s *captureSender) Send(ctx context.Context, req *netloom.Request) (*netloom.Response, error) {
	response, err := s.engine.Send(ctx, req)
	if response != nil && s.store != nil {
		id, captureErr := s.store.Record(traffic.Capture{URL: req.URL, Method: req.Method, Proto: response.HTTPVersion, RequestHeaders: http.Header(response.RequestHeaders), RequestBody: req.Body, StatusCode: response.StatusCode, ResponseHeaders: http.Header(response.Header), ResponseBody: response.Body, Complete: err == nil && !response.BodyTruncated})
		if captureErr != nil {
			return response, captureErr
		}
		s.ids = append(s.ids, id)
	}
	return response, err
}
