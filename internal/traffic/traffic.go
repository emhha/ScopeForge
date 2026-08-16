// Package traffic 是流量捕获与审计(docs/04 §3.3)。
//
// mitmproxy 捕获全部经过隧道的流量:
//   - 每条记录: flowRef / method / host / path / status / req/resp 摘要
//   - 落 events 表(kind: traffic),前端可按 flowRef 查看详情(M3)
//   - 敏感信息(cookie/凭据)默认脱敏存储,审计可见
//
// 本包提供:
//   - Recorder:审计记录器(脱敏 + events 落库 + 进程内队列)
//   - Proxy:内置 HTTP 正向代理(纯 Go,无外部依赖;mitmproxy 不可用环境的兜底,
//     也作为测试用后端)。真实 mitmproxy 由 route 包按需检测接入(见
//     internal/route 的 mitmproxy 后端)。
package traffic

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"scopeforge/internal/event"
	"scopeforge/internal/reasonix/secrets"
)

// Flow 是一条流量审计记录。
type Flow struct {
	FlowRef string `json:"flow_ref"`
	TS      int64  `json:"ts"`
	Method  string `json:"method"`
	Host    string `json:"host"`
	Path    string `json:"path"`
	Status  int    `json:"status,omitempty"`
	ReqHead string `json:"req_head,omitempty"`  // 脱敏后的请求头摘要
	RespHead string `json:"resp_head,omitempty"` // 脱敏后的响应头摘要
}

// Recorder 记录流量到 events(kind=traffic)与进程内审计队列。
type Recorder struct {
	mu       sync.Mutex
	sink     event.Sink
	challenge string
	flows    []Flow
	maxAudit int
	next     int
}

// NewRecorder 构建 Recorder。
func NewRecorder(sink event.Sink, challengeID string) *Recorder {
	return &Recorder{sink: sink, challenge: challengeID, maxAudit: 500}
}

// Record 记录一条 flow(自动脱敏)。
func (r *Recorder) Record(f Flow) {
	if f.FlowRef == "" {
		r.mu.Lock()
		r.next++
		f.FlowRef = fmt.Sprintf("flow-%d", r.next)
		r.mu.Unlock()
	}
	if f.TS == 0 {
		f.TS = time.Now().Unix()
	}
	f.ReqHead = Redact(f.ReqHead)
	f.RespHead = Redact(f.RespHead)
	r.mu.Lock()
	r.flows = append(r.flows, f)
	if len(r.flows) > r.maxAudit {
		r.flows = r.flows[len(r.flows)-r.maxAudit:]
	}
	r.mu.Unlock()
	if r.sink != nil {
		r.sink.Emit(event.Event{Kind: event.KindTraffic, ChallengeID: r.challenge,
			Payload: map[string]any{
				"flow_ref": f.FlowRef, "ts": f.TS, "method": f.Method,
				"host": f.Host, "path": f.Path, "status": f.Status,
				"req_head": f.ReqHead, "resp_head": f.RespHead,
			}})
	}
}

// Flows 返回审计队列副本(按时间序)。
func (r *Recorder) Flows() []Flow {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Flow, len(r.flows))
	copy(out, r.flows)
	return out
}

// Redact 脱敏敏感信息(cookie/凭据/Authorization),审计可见。
func Redact(s string) string {
	s = secrets.RedactCredentials(s)
	// Cookie 行整体脱敏(名称保留,值打码)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "cookie:") || strings.HasPrefix(lower, "set-cookie:") {
			lines[i] = line[:strings.Index(line, ":")+1] + " [redacted]"
		}
	}
	return strings.Join(lines, "\n")
}

// Proxy 是内置 HTTP 正向代理:捕获经过的 HTTP 与 CONNECT(HTTPS)流量。
// 出口边界:元数据端点(169.254.169.254/metadata.google.internal)永远拒绝
// (docs/04 §5.3 网络边界;目标白名单由 ScopeGate 在工具层控制)。
type Proxy struct {
	Recorder  *Recorder
	ln        net.Listener
	mu        sync.Mutex
	conns     int
	BlockMeta bool // 拒绝元数据端点(默认 true)
}

// StartProxy 启动代理监听。返回监听地址(如 127.0.0.1:12345)。
func StartProxy(rec *Recorder, addr string) (*Proxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{Recorder: rec, ln: ln, BlockMeta: true}
	go p.serve()
	return p, nil
}

// Addr 返回监听地址。
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Close 关闭代理。
func (p *Proxy) Close() { p.ln.Close() }

func (p *Proxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns++
		p.mu.Unlock()
		go p.handle(conn)
	}
}

func (p *Proxy) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	// CONNECT 隧道:记录元数据后透传(不 MITM TLS 内容)
	if req.Method == http.MethodConnect {
		if p.BlockMeta && BlockedMeta(req.Host) {
			fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\n\r\n")
			return
		}
		flow := Flow{Method: "CONNECT", Host: req.Host, Path: "", Status: 200}
		if p.Recorder != nil {
			p.Recorder.Record(flow)
		}
		upstream, err := net.DialTimeout("tcp", req.Host, 5*time.Second)
		if err != nil {
			fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}
		defer upstream.Close()
		fmt.Fprintf(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(upstream, br)
		io.Copy(conn, upstream)
		return
	}
	// 普通 HTTP:解析绝对 URL 转发
	target := req.URL
	if !target.IsAbs() {
		target = &url.URL{Scheme: "http", Host: req.Host, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	}
	if p.BlockMeta && BlockedMeta(target.Host) {
		return
	}
	flow := Flow{Method: req.Method, Host: target.Host, Path: target.Path, ReqHead: headSummary(req.Header)}
	upstream, err := net.DialTimeout("tcp", target.Host, 5*time.Second)
	if err != nil {
		if p.Recorder != nil {
			flow.Status = 502
			p.Recorder.Record(flow)
		}
		return
	}
	defer upstream.Close()
	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	outReq.URL = target
	if err := outReq.Write(upstream); err != nil {
		return
	}
	brr := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(brr, outReq)
	if err != nil {
		return
	}
	flow.Status = resp.StatusCode
	flow.RespHead = headSummary(resp.Header)
	if p.Recorder != nil {
		p.Recorder.Record(flow)
	}
	resp.Write(conn)
}

// BlockedMeta 元数据端点永远拒绝(docs/04 §5.3;与 executor.MetaDataBlocked 同语义)。
// host 可为 host:port 或裸 host。
func BlockedMeta(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h == "169.254.169.254" || h == "metadata.google.internal"
}

// headSummary 序列化请求/响应头(值已脱敏由 Redact 负责)。
func headSummary(h http.Header) string {
	var b strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	return b.String()
}

var _ = context.Background
