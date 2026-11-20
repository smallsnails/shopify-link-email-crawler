package main

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
)

type queueItem struct {
	url   string
	depth int
}

type fetchResult struct {
	item  queueItem
	page  *PageResult
	links []*Link
}

// Crawler 负责按域名抓取页面、链接与邮箱。
type Crawler struct {
	cfg     *Config
	fetcher *Fetcher

	mu        sync.Mutex
	pages     []*PageResult
	pageKeys  map[string]int
	links     []*Link
	linkKeys  map[string]bool
	emails    []*EmailHit
	emailKeys map[string]bool
	domains   []string
	domainSet map[string]bool
}

func NewCrawler(cfg *Config) *Crawler {
	return &Crawler{
		cfg:       cfg,
		fetcher:   NewFetcher(cfg.Timeout, cfg.UA, cfg.Delay, cfg.Robots),
		pageKeys:  map[string]int{},
		linkKeys:  map[string]bool{},
		emailKeys: map[string]bool{},
		domainSet: map[string]bool{},
	}
}

// CrawlSite 抓取一个站点：首页 + 深度限制内的链接页。
func (c *Crawler) CrawlSite(ctx context.Context, seed string) error {
	u, err := url.Parse(seed)
	if err != nil {
		return err
	}
	root := rootDomain(u.Hostname())
	if root == "" {
		return fmt.Errorf("无法解析主域名: %s", seed)
	}

	c.mu.Lock()
	if !c.domainSet[root] {
		c.domainSet[root] = true
		c.domains = append(c.domains, root)
	}
	c.mu.Unlock()

	visited := map[string]bool{canonicalURL(seed): true}
	current := []queueItem{{url: seed, depth: 0}}
	fetched := 0

	for len(current) > 0 {
		remaining := c.cfg.MaxPages - fetched
		if remaining <= 0 {
			break
		}
		if len(current) > remaining {
			current = current[:remaining]
		}
		fetched += len(current)

		results := c.fetchLevel(ctx, current, root)

		var next []queueItem
		for _, r := range results {
			if r.page == nil {
				continue
			}
			if r.page.Err != "" {
				fmt.Printf("  ✗ %-3s %-90s %s\n", "-", shorten(r.page.URL, 90), r.page.Err)
				continue
			}
			fmt.Printf("  ✓ %-3d %-90s 链接 %d / 邮箱 %d\n",
				r.page.StatusCode, shorten(r.page.URL, 90), r.page.LinkCount, r.page.EmailCount)

			if r.item.depth >= c.cfg.Depth {
				continue
			}
			for _, l := range r.links {
				if !l.Crawlable {
					continue
				}
				key := canonicalURL(l.URL)
				if visited[key] {
					continue
				}
				visited[key] = true
				next = append(next, queueItem{url: l.URL, depth: r.item.depth + 1})
			}
		}
		current = next
	}

	nPages, nLinks, nMail := c.countFor(root)
	fmt.Printf("  站点 %s 完成：页面 %d，链接 %d，邮箱 %d\n", root, nPages, nLinks, nMail)
	return nil
}

func (c *Crawler) fetchLevel(ctx context.Context, items []queueItem, root string) []fetchResult {
	results := make([]fetchResult, len(items))
	sem := make(chan struct{}, c.cfg.Workers)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it queueItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = c.processPage(ctx, it, root)
		}(i, it)
	}
	wg.Wait()
	return results
}

func (c *Crawler) processPage(ctx context.Context, item queueItem, root string) fetchResult {
	pg := &PageResult{RootDomain: root, URL: item.url}
	res := fetchResult{item: item, page: pg}

	if !c.fetcher.Allowed(ctx, item.url) {
		pg.Err = "robots.txt 禁止抓取"
		c.store(pg, nil, nil)
		return res
	}

	resp, err := c.fetcher.Get(ctx, item.url)
	if err != nil {
		pg.Err = err.Error()
		c.store(pg, nil, nil)
		return res
	}
	pg.StatusCode = resp.StatusCode
	pg.FinalURL = resp.FinalURL

	if resp.StatusCode >= 400 {
		pg.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		c.store(pg, nil, nil)
		return res
	}
	ct := strings.ToLower(resp.ContentType)
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "xml") {
		pg.Err = "非 HTML 内容：" + ct
		c.store(pg, nil, nil)
		return res
	}

	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if derr != nil {
		pg.Err = "HTML 解析失败: " + derr.Error()
		c.store(pg, nil, nil)
		return res
	}

	pg.Fetched = true
	pg.Title = strings.Join(strings.Fields(doc.Find("title").First().Text()), " ")
	if len(pg.Title) > 120 {
		pg.Title = pg.Title[:120] + "..."
	}

	links, _ := ExtractLinks(doc, resp.FinalURL, root, c.cfg)
	emails := ExtractEmailsFromPage(resp.Body, doc, resp.FinalURL, root, c.cfg.IgnoreDomains)

	pg.LinkCount = len(links)
	pg.EmailCount = len(emails)

	c.store(pg, links, emails)
	res.links = links
	return res
}

func (c *Crawler) store(pg *PageResult, links []*Link, emails []*EmailHit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := canonicalURL(pg.URL)
	if idx, ok := c.pageKeys[key]; ok {
		if c.pages[idx].Fetched && !pg.Fetched {
			return
		}
		c.pages[idx] = pg
	} else {
		c.pageKeys[key] = len(c.pages)
		c.pages = append(c.pages, pg)
	}

	for _, l := range links {
		lk := l.SourcePage + "|" + canonicalURL(l.URL)
		if c.linkKeys[lk] {
			continue
		}
		c.linkKeys[lk] = true
		c.links = append(c.links, l)
	}

	for _, e := range emails {
		ek := e.Domain + "|" + e.Email
		if c.emailKeys[ek] {
			continue
		}
		c.emailKeys[ek] = true
		c.emails = append(c.emails, e)
	}
}

func (c *Crawler) countFor(root string) (pages, links, emails int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pages {
		if p.RootDomain == root && p.Fetched {
			pages++
		}
	}
	for _, l := range c.links {
		if l.RootDomain == root {
			links++
		}
	}
	for _, e := range c.emails {
		if e.Domain == root {
			emails++
		}
	}
	return
}

// ---------------------------------------------------------------------------
// 链接可用性校验
// ---------------------------------------------------------------------------

func aliveLabel(status int) string {
	switch {
	case status == 0:
		return "未校验"
	case status >= 200 && status < 300:
		return "有效"
	case status >= 300 && status < 400:
		return "有效(跳转)"
	case status == 401 || status == 403 || status == 429:
		return "受限"
	case status == 404 || status == 410:
		return "无效"
	case status >= 500:
		return "服务器错误"
	default:
		return fmt.Sprintf("异常(%d)", status)
	}
}

type probeResult struct {
	status int
	alive  string
}

// VerifyLinks 对所有未抓取的链接做 HEAD/GET 存活探测，并回写状态。
func (c *Crawler) VerifyLinks(ctx context.Context) {
	c.mu.Lock()
	links := append([]*Link{}, c.links...)
	pages := append([]*PageResult{}, c.pages...)
	c.mu.Unlock()

	fetched := map[string]*PageResult{}
	for _, p := range pages {
		fetched[canonicalURL(p.URL)] = p
	}

	var targets []string
	seen := map[string]bool{}
	for _, l := range links {
		if p, ok := fetched[canonicalURL(l.URL)]; ok && p.StatusCode > 0 {
			continue
		}
		// 默认不探测外站链接：多为社交平台/合作方站点，易触发风控且拖慢整体速度
		if l.Scope == "external" && !c.cfg.VerifyExternal {
			continue
		}
		k := canonicalURL(l.URL)
		if seen[k] {
			continue
		}
		seen[k] = true
		targets = append(targets, l.URL)
	}
	if c.cfg.MaxLinksVerify > 0 && len(targets) > c.cfg.MaxLinksVerify {
		targets = targets[:c.cfg.MaxLinksVerify]
	}

	results := make([]probeResult, len(targets))
	sem := make(chan struct{}, c.cfg.Workers)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				results[i] = probeResult{0, "未校验"}
				return
			}
			resp, err := c.fetcher.Head(ctx, target)
			if err != nil {
				results[i] = probeResult{0, "无法访问"}
				return
			}
			results[i] = probeResult{resp.StatusCode, aliveLabel(resp.StatusCode)}
		}(i, target)
	}
	wg.Wait()

	status := make(map[string]probeResult, len(targets))
	for i, t := range targets {
		status[canonicalURL(t)] = results[i]
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.links {
		if p, ok := fetched[canonicalURL(l.URL)]; ok && p.StatusCode > 0 {
			l.Status = p.StatusCode
			l.Alive = aliveLabel(p.StatusCode)
			l.Crawled = p.Fetched
			continue
		}
		if pr, ok := status[canonicalURL(l.URL)]; ok {
			l.Status = pr.status
			l.Alive = pr.alive
		}
	}
	fmt.Printf("  已校验 %d 个链接\n", len(targets))
}

// Snapshot 生成导出用的数据快照。
func (c *Crawler) Snapshot() *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := &Snapshot{
		Domains: append([]string{}, c.domains...),
		Pages:   append([]*PageResult{}, c.pages...),
		Links:   append([]*Link{}, c.links...),
		Emails:  append([]*EmailHit{}, c.emails...),
	}
	for _, p := range s.Pages {
		if p.Fetched {
			s.PageOK++
		}
	}
	sortEmails(s.Emails)
	return s
}
