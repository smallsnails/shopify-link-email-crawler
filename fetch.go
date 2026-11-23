package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Response 精简后的 HTTP 响应。
type Response struct {
	Body        []byte
	FinalURL    string
	StatusCode  int
	ContentType string
	Header      http.Header
}

// Fetcher 带“单域名串行限速 + 429 冷却 + 重试 + robots”的 HTTP 客户端。
type Fetcher struct {
	client  *http.Client
	ua      string
	delay   time.Duration
	maxBody int64

	mu       sync.Mutex
	lastHit  map[string]time.Time // 每个域名的下一次可用时间（含预约）
	cooldown map[string]time.Time // 被 429/503 惩罚的冷却截止时间

	robotsMu  sync.Mutex
	robots    map[string]*robotsRules
	useRobots bool
}

func NewFetcher(timeout time.Duration, ua string, delay time.Duration, useRobots bool) *Fetcher {
	tr := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
		Proxy:               http.ProxyFromEnvironment,
	}
	return &Fetcher{
		client: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 8 {
					return errors.New("重定向次数过多")
				}
				return nil
			},
		},
		ua:        ua,
		delay:     delay,
		maxBody:   6 << 20, // 6MB
		lastHit:   map[string]time.Time{},
		cooldown:  map[string]time.Time{},
		robots:    map[string]*robotsRules{},
		useRobots: useRobots,
	}
}

// waitTurn 为某个域名预约下一个请求时隙，实现“同域串行 + 固定间隔”，
// 即使上层并发，也不会把同一域名打出并发洪峰。
func (f *Fetcher) waitTurn(ctx context.Context, host string) error {
	f.mu.Lock()
	now := time.Now()
	var wait time.Duration
	if t, ok := f.cooldown[host]; ok && t.After(now) {
		wait = t.Sub(now)
	}
	if last, ok := f.lastHit[host]; ok {
		// lastHit 是上一个请求预约的发送时点，下一个请求应排在其 delay 之后；
		// 并发突发时 lastHit 会指向未来，从而把请求均匀铺开。
		if w := time.Until(last) + f.delay; w > wait {
			wait = w
		}
	}
	f.lastHit[host] = now.Add(wait)
	f.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func (f *Fetcher) coolDown(host string, retryAfter time.Duration, attempt int) {
	d := retryAfter
	if d <= 0 {
		d = time.Duration(2*(attempt+1)) * time.Second
	}
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	f.mu.Lock()
	f.cooldown[host] = time.Now().Add(d)
	f.mu.Unlock()
}

// Get 抓取页面。429/5xx 会冷却重试，重试仍失败则返回带状态码的响应，
// 便于上层把该页标记为“受限”而不是当作网络错误丢弃。
func (f *Fetcher) Get(ctx context.Context, rawurl string) (*Response, error) {
	return f.do(ctx, http.MethodGet, rawurl, 0, 3)
}

// Probe 抓取优先路径探测页：不重试。探测页大概率大量 404/超时，
// 每个失败都重试 3 次会把死站点拖到几十分钟。
func (f *Fetcher) Probe(ctx context.Context, rawurl string) (*Response, error) {
	return f.do(ctx, http.MethodGet, rawurl, 0, 1)
}

// Head 只探测状态码；服务端不支持 HEAD 时回退 GET。校验场景不重试，避免整体变慢。
func (f *Fetcher) Head(ctx context.Context, rawurl string) (*Response, error) {
	resp, err := f.do(ctx, http.MethodHead, rawurl, 0, 1)
	if err != nil {
		return f.do(ctx, http.MethodGet, rawurl, 256<<10, 1)
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return f.do(ctx, http.MethodGet, rawurl, 256<<10, 1)
	}
	return resp, nil
}

func (f *Fetcher) do(ctx context.Context, method, rawurl string, limit int64, attempts int) (*Response, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("不支持的协议: %s", u.Scheme)
	}
	if limit <= 0 {
		limit = f.maxBody
	}
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
			}
		}
		if err := f.waitTurn(ctx, u.Host); err != nil {
			return nil, err
		}

		req, reqErr := http.NewRequestWithContext(ctx, method, rawurl, nil)
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("User-Agent", f.ua)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8")
		req.Header.Set("Cache-Control", "no-cache")

		resp, doErr := f.client.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			// Shopify / Cloudflare 的典型反爬响应：冷却后重试；
			// 部分店铺对特定路径（如 /collections/*）长期返回 429，重试无意义。
			if attempt >= attempts-1 {
				return &Response{
					Body: body, FinalURL: resp.Request.URL.String(), StatusCode: resp.StatusCode,
					ContentType: resp.Header.Get("Content-Type"), Header: resp.Header,
				}, nil
			}
			f.coolDown(u.Host, parseRetryAfter(resp.Header.Get("Retry-After")), attempt)
			lastErr = errors.New("HTTP 429 触发限流")
			continue
		case resp.StatusCode >= 500:
			if attempt >= attempts-1 {
				return &Response{
					Body: body, FinalURL: resp.Request.URL.String(), StatusCode: resp.StatusCode,
					ContentType: resp.Header.Get("Content-Type"), Header: resp.Header,
				}, nil
			}
			f.coolDown(u.Host, 0, attempt)
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		return &Response{
			Body:        body,
			FinalURL:    resp.Request.URL.String(),
			StatusCode:  resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Header:      resp.Header,
		}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("请求失败")
	}
	return nil, lastErr
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// robots.txt（简化实现：只处理 User-agent: * 组的 Disallow / Allow 前缀匹配）
// ---------------------------------------------------------------------------

type robotsRules struct {
	disallow []string
	allow    []string
}

func (f *Fetcher) rulesFor(ctx context.Context, rawurl string) *robotsRules {
	u, err := url.Parse(rawurl)
	if err != nil {
		return &robotsRules{}
	}
	origin := u.Scheme + "://" + u.Host

	f.robotsMu.Lock()
	r, ok := f.robots[origin]
	f.robotsMu.Unlock()
	if ok {
		return r
	}

	rr := &robotsRules{}
	resp, err := f.do(ctx, http.MethodGet, origin+"/robots.txt", 512<<10, 1)
	if err == nil && resp.StatusCode < 400 {
		rr = parseRobots(string(resp.Body))
	}

	f.robotsMu.Lock()
	f.robots[origin] = rr
	f.robotsMu.Unlock()
	return rr
}

func parseRobots(body string) *robotsRules {
	rr := &robotsRules{}
	inStar := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "user-agent":
			inStar = val == "*"
		case "disallow":
			if inStar && val != "" {
				rr.disallow = append(rr.disallow, val)
			}
		case "allow":
			if inStar && val != "" {
				rr.allow = append(rr.allow, val)
			}
		}
	}
	return rr
}

// Allowed 判断 URL 是否允许抓取。
func (f *Fetcher) Allowed(ctx context.Context, rawurl string) bool {
	if !f.useRobots {
		return true
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return true
	}
	rr := f.rulesFor(ctx, rawurl)
	if rr == nil || len(rr.disallow) == 0 {
		return true
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	// 最长匹配优先；等长时 Allow 胜出
	bestLen, allowed := 0, true
	for _, a := range rr.allow {
		if strings.HasPrefix(p, a) && len(a) >= bestLen {
			bestLen, allowed = len(a), true
		}
	}
	for _, d := range rr.disallow {
		if strings.HasPrefix(p, d) && len(d) > bestLen {
			bestLen, allowed = len(d), false
		}
	}
	return allowed
}
