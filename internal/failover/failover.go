// Package failover 是 Provider 自动切换(docs/06 §4.1 LLM 网关故障,
// ⚑ Threonine 单点教训)。
//
// 语义:
//   - 健康探测先行:每次请求前 2s 探测当前 provider 端点,
//     不可达(连接失败/5xx)→ 立即切备份(不等 provider 内部 10 次重试,§4.1 快速自愈)
//   - 连接级失败(Stream 返回错误)→ 切换备份并重试整个请求
//   - 流中失败(chunk.Err)→ 标记当前 provider 失效,下个请求切备份
//     (已消费的流无法重试;executor 按轮落库,下轮继续 — 无会话损坏,
//     M1 §7 会话损坏回滚兜底)
//   - 全部失败 → 返回错误(上层 BudgetMeter/重试策略处理)
package failover

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"scopeforge/internal/reasonix/provider"
)

// Provider 是 failover 包装。
type Provider struct {
	mu           sync.Mutex
	primary      provider.Provider
	backups      []provider.Provider
	index        int // 当前使用的 provider 下标(0 = primary)
	probeURL     string
	probeTimeout time.Duration
}

// New 构建 failover 包装。probeURL 非空时启用健康探测先行(建议传 primary 的 base_url);
// backups 为空时退化为直通。
func New(primary provider.Provider, backups []provider.Provider, probeURL string) *Provider {
	if primary == nil {
		return nil
	}
	return &Provider{primary: primary, backups: backups, probeURL: probeURL, probeTimeout: 2 * time.Second}
}

// Name 返回当前 provider 名(会话记录用)。
func (p *Provider) Name() string { return p.current().Name() }

// CurrentName 当前生效 provider 名(诊断/账本)。
func (p *Provider) CurrentName() string { return p.current().Name() }

// Stream 流式补全:健康探测 + 连接级失败自动切换(最多 primary+backups 次)。
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	total := 1 + len(p.backups)
	for attempt := 0; attempt < total; attempt++ {
		cur := p.current()
		// 健康探测先行(仅 primary):主网关不可达立即切换(原子操作,防并发跳级),
		// 避免 provider 内部 10 次重试拖慢自愈;备份失败走 Stream err 路径
		if p.switchIfPrimaryUnhealthy(ctx) {
			continue
		}
		ch, err := cur.Stream(ctx, req)
		if err != nil {
			p.failover()
			continue // 切换备份重试整个请求
		}
		out := make(chan provider.Chunk, 32)
		go func(cur provider.Provider) {
			defer close(out)
			for c := range ch {
				if c.Err != nil {
					p.failover() // 流中失败:标记切换(下个请求生效)
				}
				select {
				case out <- c:
				case <-ctx.Done():
					return
				}
			}
		}(cur)
		return out, nil
	}
	return nil, fmt.Errorf("failover: all %d providers failed", total)
}

// switchIfPrimaryUnhealthy 原子检查+切换:主 provider 探测不可达 → 切到第一个备份。
// 锁内完成(探测+index 变更),并发 Stream 不会跳过备份(§4.1 防抖动)。
func (p *Provider) switchIfPrimaryUnhealthy(ctx context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.index != 0 || p.probeURL == "" || len(p.backups) == 0 {
		return false
	}
	if !p.probe(ctx) {
		p.index = 1
		return true
	}
	return false
}

// probe 健康探测当前端点(2s 超时;连接失败或 5xx = 不可达)。
func (p *Provider) probe(ctx context.Context) bool {
	c := &http.Client{Timeout: p.probeTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.probeURL, nil)
	if err != nil {
		return false
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// current 返回当前 provider(线程安全)。
func (p *Provider) current() provider.Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.index == 0 {
		return p.primary
	}
	return p.backups[p.index-1]
}

// failover 切换到下一个备份(不循环回 primary,防抖动风暴)。
func (p *Provider) failover() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.backups) == 0 {
		return
	}
	if p.index < len(p.backups) {
		p.index++
	}
}
