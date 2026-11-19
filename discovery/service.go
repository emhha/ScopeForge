// Package discovery connects configured mapping sources to netloom's provider clients.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/emhha/netloom/httpclient"
	"github.com/emhha/netloom/mapping"
)

var providers = []string{"fofa", "hunter", "quake"}
var endpoints = map[string]string{"fofa": "https://fofa.info/api/v1/search/all", "hunter": "https://hunter.qianxin.com/openApi/search", "quake": "https://quake.360.net/api/v3/search/quake_service"}

type Source struct {
	Enabled        bool   `json:"enabled"`
	Endpoint       string `json:"endpoint"`
	Proxy          string `json:"proxy"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Key            string `json:"key,omitempty"`
	Email          string `json:"email,omitempty"`
}
type SourceView struct {
	ID             string `json:"id"`
	Enabled        bool   `json:"enabled"`
	Endpoint       string `json:"endpoint"`
	Proxy          string `json:"proxy"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	KeySet         bool   `json:"key_set"`
	KeyFromEnv     bool   `json:"key_from_env"`
	Email          string `json:"email"`
}
type Patch struct {
	Enabled        *bool   `json:"enabled"`
	Endpoint       *string `json:"endpoint"`
	Proxy          *string `json:"proxy"`
	TimeoutSeconds *int    `json:"timeout_seconds"`
	Key            *string `json:"key"`
	Email          *string `json:"email"`
}
type Service struct {
	db     *db.DB
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
	slots  chan struct{}
	proxy  func() string
}

func New(d *db.DB, proxy func() string) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{db: d, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 4), proxy: proxy}
}
func (s *Service) Close() { s.mu.Lock(); s.closed = true; s.cancel(); s.mu.Unlock(); s.wg.Wait() }
func (s *Service) load(id string) (Source, error) {
	endpoint, ok := endpoints[id]
	if !ok {
		return Source{}, errors.New("未知测绘数据源")
	}
	c := Source{Endpoint: endpoint, TimeoutSeconds: 30}
	raw, exists, err := s.db.GetSetting("mapping.source." + id)
	if err != nil {
		return c, err
	}
	if exists {
		err = json.Unmarshal([]byte(raw), &c)
	}
	if !exists && strings.TrimSpace(os.Getenv("ARTEX_"+strings.ToUpper(id)+"_KEY")) != "" {
		c.Enabled = true
	}
	return c, err
}
func withEnvironment(id string, c Source) Source {
	if c.Key == "" {
		c.Key = strings.TrimSpace(os.Getenv("ARTEX_" + strings.ToUpper(id) + "_KEY"))
	}
	if id == "fofa" && c.Email == "" {
		c.Email = strings.TrimSpace(os.Getenv("ARTEX_FOFA_EMAIL"))
	}
	return c
}
func view(id string, c Source) SourceView {
	effective := withEnvironment(id, c)
	return SourceView{ID: id, Enabled: c.Enabled, Endpoint: c.Endpoint, Proxy: c.Proxy, TimeoutSeconds: c.TimeoutSeconds, KeySet: effective.Key != "", KeyFromEnv: c.Key == "" && effective.Key != "", Email: effective.Email}
}
func (s *Service) Sources() ([]SourceView, error) {
	out := []SourceView{}
	for _, id := range providers {
		c, err := s.load(id)
		if err != nil {
			return nil, err
		}
		out = append(out, view(id, c))
	}
	return out, nil
}
func (s *Service) Update(id string, p Patch) (SourceView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(id)
	if err != nil {
		return SourceView{}, err
	}
	if p.Enabled != nil {
		c.Enabled = *p.Enabled
	}
	if p.Endpoint != nil {
		c.Endpoint = strings.TrimSpace(*p.Endpoint)
		if c.Endpoint == "" {
			c.Endpoint = endpoints[id]
		}
	}
	if p.Proxy != nil {
		c.Proxy = strings.TrimSpace(*p.Proxy)
	}
	if p.TimeoutSeconds != nil {
		c.TimeoutSeconds = *p.TimeoutSeconds
	}
	if p.Key != nil {
		c.Key = strings.TrimSpace(*p.Key)
	}
	if p.Email != nil {
		c.Email = strings.TrimSpace(*p.Email)
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 120 {
		return SourceView{}, errors.New("超时须为 1–120 秒")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
		return SourceView{}, errors.New("数据源地址须为 HTTP/HTTPS 且不含凭据、查询参数和片段")
	}
	if c.Proxy != "" {
		if _, err := traffic.ValidateProxyURL(c.Proxy); err != nil {
			return SourceView{}, err
		}
	}
	if len(c.Key) > 8192 || len(c.Email) > 512 || strings.ContainsAny(c.Key+c.Email, "\r\n\x00") {
		return SourceView{}, errors.New("凭据格式无效")
	}
	if c.Enabled && withEnvironment(id, c).Key == "" {
		return SourceView{}, errors.New("启用前请配置 API Key")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return SourceView{}, err
	}
	if err = s.db.SetSetting("mapping.source."+id, string(raw)); err != nil {
		return SourceView{}, err
	}
	return view(id, c), nil
}

type Input struct {
	Query    string `json:"query"`
	Provider string `json:"provider"`
	Limit    int    `json:"limit"`
	Native   bool   `json:"native"`
}
type Output struct {
	Results  []mapping.Result `json:"results"`
	Warnings []string         `json:"warnings"`
}
type secrets map[string]string

func (s secrets) Get(ctx context.Context, key string) ([]byte, error) {
	return []byte(s[key]), ctx.Err()
}
func (s *Service) Search(ctx context.Context, in Input) (*Output, error) {
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len(in.Query) > 4096 {
		return nil, errors.New("查询须为 1–4096 字节")
	}
	if in.Provider == "" {
		in.Provider = "all"
	}
	if in.Provider != "all" {
		if _, ok := endpoints[in.Provider]; !ok {
			return nil, errors.New("未知测绘数据源")
		}
	}
	if in.Native && in.Provider == "all" {
		return nil, errors.New("原生语法须选择单一数据源")
	}
	if !in.Native && net.ParseIP(in.Query) == nil && (strings.ContainsAny(in.Query, " /\\\"'=:()\t\r\n") || !strings.Contains(in.Query, ".")) {
		return nil, errors.New("通用查询须为域名或 IP；复杂语法请选择单源并启用 native")
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.Limit < 1 || in.Limit > 200 {
		return nil, errors.New("limit 须为 1–200")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("测绘服务已关闭")
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := &Output{Results: []mapping.Result{}, Warnings: []string{}}
	seen := map[string]int{}
	for _, id := range providers {
		if in.Provider != "all" && in.Provider != id {
			continue
		}
		config, err := s.load(id)
		if err != nil {
			return nil, err
		}
		config = withEnvironment(id, config)
		if !config.Enabled || config.Key == "" {
			if in.Provider != "all" {
				return nil, errors.New("该数据源未启用或未配置 API Key")
			}
			continue
		}
		proxy := config.Proxy
		if proxy == "" && s.proxy != nil {
			proxy = s.proxy()
		}
		client := httpclient.NewSession(httpclient.Options{Proxy: proxy, Timeout: time.Duration(config.TimeoutSeconds) * time.Second, DisableRetries: true})
		keys := secrets{id + "_key": config.Key, "fofa_email": config.Email}
		var engine mapping.Engine
		switch id {
		case "fofa":
			engine = mapping.NewFofaEngine(client, keys, config.Endpoint)
		case "hunter":
			engine = mapping.NewHunterEngine(client, keys, config.Endpoint)
		case "quake":
			engine = mapping.NewQuakeEngineAt(client, keys, config.Endpoint)
		}
		query := in.Query
		if !in.Native {
			query = engine.ConvertQuery(query)
		}
		results, err := engine.Search(ctx, query, in.Limit)
		client.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: 查询失败，请检查配置、语法或账户额度", id))
			continue
		}
		for _, result := range results {
			key := strings.ToLower(result.Host) + "|" + result.IP + "|" + strconv.Itoa(result.Port) + "|" + result.Scheme
			if index, exists := seen[key]; exists {
				if !slices.Contains(out.Results[index].Sources, id) {
					out.Results[index].Sources = append(out.Results[index].Sources, id)
				}
				continue
			}
			if len(out.Results) >= in.Limit {
				continue
			}
			seen[key] = len(out.Results)
			result.Raw = nil
			result.Sources = []string{id}
			out.Results = append(out.Results, result)
		}
	}
	if len(out.Results) == 0 && len(out.Warnings) == 0 {
		return nil, errors.New("没有启用的测绘数据源，请在系统配置中添加 API Key")
	}
	return out, nil
}
