package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type queueItem struct {
	url   string
	depth int
	probe bool // 优先路径探测页（未命中时不入库）
}

// DefaultPriorityPaths 联系邮箱最常出现的页面路径（Shopify + 通用独立站），
// 每个站点会先探测这些路径，命中后再抓 header/footer/nav 链接页。
var DefaultPriorityPaths = []string{
	// 联系页
	"/contact", "/contact-us", "/contactus",
	"/pages/contact", "/pages/contact-us",
	// 关于/公司
	"/about", "/about-us", "/aboutus", "/company-profile",
	"/pages/about", "/pages/about-us",
	// 帮助/FAQ
	"/faq", "/faqs", "/pages/faq", "/help", "/help-center", "/support",
	// 政策页（通用路径）
	"/privacy-policy", "/terms-of-service", "/refund-policy", "/shipping-policy",
	// 政策页（Shopify 固定路径）
	"/policies/privacy-policy", "/policies/refund-policy",
	"/policies/terms-of-service", "/policies/shipping-policy",
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

	linkCount     map[string]int  // 每个站点已收录的链接数
	linkTruncated map[string]bool // 链接清单是否被截断
	blocked       []string        // 首页持续 429 的站点（等待批次结束后冷却重试）
}

func NewCrawler(cfg *Config) *Crawler {
	return &Crawler{
		cfg:           cfg,
		fetcher:       NewFetcher(cfg.Timeout, cfg.UA, cfg.Delay, cfg.GlobalDelay, cfg.Robots),
		pageKeys:      map[string]int{},
		linkKeys:      map[string]bool{},
		emailKeys:     map[string]bool{},
		domainSet:     map[string]bool{},
		linkCount:     map[string]int{},
		linkTruncated: map[string]bool{},
	}
}

// CrawlSite 抓取一个站点：首页 + 优先路径（--all-links 开启时含链接页）。
// 首页处理策略：
//   - 网络层失败（DNS 挂/超时/拒连）→ 站点真死，短路跳过，省掉 25 个探测请求
//   - HTTP 429 → 冷却 10s/20s 重试最多 2 次，仍失败才放弃（Cloudflare 的 IP
//     惩罚一般几十秒解除，不能一棒子打死）
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
	fetched := 0

	// ---- 首页（含 429 冷却重试）----
	home := c.fetchLevel(ctx, []queueItem{{url: seed, depth: 0}}, root)[0]
	if home.page == nil {
		return fmt.Errorf("首页抓取异常: %s", seed)
	}
	pg := home.page
	for retry := 0; pg.Err != "" && pg.StatusCode == http.StatusTooManyRequests && retry < 2; retry++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := time.Duration(10*(retry+1)) * time.Second
		fmt.Printf("  首页被限流(HTTP 429)，冷却 %v 后重试 (%d/2) ...\n", wait, retry+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		home = c.fetchLevel(ctx, []queueItem{{url: seed, depth: 0}}, root)[0]
		pg = home.page
	}
	fetched = 1

	switch {
	case ctx.Err() != nil:
		// 用户 Ctrl+C：安静退出，不给 50 个站点各刷一行“无法访问”
		return ctx.Err()
	case pg.Err != "" && pg.StatusCode == 0:
		fmt.Printf("  ✗ 首页无法访问（%s），整站跳过\n", pg.Err)
		return nil
	case pg.Err != "":
		if pg.StatusCode == http.StatusTooManyRequests {
			// 持续 429：记入待重试名单，等批次跑完 IP 冷却后再补一轮
			fmt.Printf("  ⏸ 首页持续 HTTP 429，记入待重试名单（批次结束后自动补抓）\n")
			c.mu.Lock()
			dup := false
			for _, b := range c.blocked {
				if b == seed {
					dup = true
					break
				}
			}
			if !dup {
				c.blocked = append(c.blocked, seed)
			}
			c.mu.Unlock()
		} else {
			fmt.Printf("  ✗ 首页 %s，跳过该站\n", pg.Err)
		}
		return nil
	}

	fmt.Printf("  ✓ %-3d %-90s 链接 %d / 邮箱 %d\n",
		pg.StatusCode, shorten(pg.URL, 90), pg.LinkCount, pg.EmailCount)

	// ---- 组装下一层队列：优先路径在前，链接页（需 --all-links）在后 ----
	var next []queueItem
	if len(c.cfg.PriorityPaths) > 0 {
		if probes := c.buildPriorityQueue(u, visited); len(probes) > 0 {
			fmt.Printf("  优先探测 %d 条联系页候选路径 ...\n", len(probes))
			next = append(next, probes...)
		}
	}
	if c.cfg.AllLinks && c.cfg.Depth >= 1 {
		for _, l := range home.links {
			if !l.Crawlable {
				continue
			}
			key := canonicalURL(l.URL)
			if visited[key] {
				continue
			}
			visited[key] = true
			next = append(next, queueItem{url: l.URL, depth: 1})
		}
	}

	// ---- 后续层级 ----
	probeHit, probeMiss := 0, 0
	for len(next) > 0 {
		remaining := c.cfg.MaxPages - fetched
		if remaining <= 0 {
			break
		}
		if len(next) > remaining {
			next = next[:remaining]
		}
		fetched += len(next)

		results := c.fetchLevel(ctx, next, root)
		next = nil
		for _, r := range results {
			if r.page == nil {
				continue
			}
			if r.page.Err != "" {
				if r.item.probe {
					// 优先路径未命中：不入库，静默跳过
					probeMiss++
					continue
				}
				fmt.Printf("  ✗ %-3s %-90s %s\n", "-", shorten(r.page.URL, 90), r.page.Err)
				continue
			}
			if r.item.probe {
				probeHit++
				fmt.Printf("  ★ %-3d %-84s 链接 %d / 邮箱 %d [优先路径]\n",
					r.page.StatusCode, shorten(r.page.URL, 84), r.page.LinkCount, r.page.EmailCount)
			} else {
				fmt.Printf("  ✓ %-3d %-90s 链接 %d / 邮箱 %d\n",
					r.page.StatusCode, shorten(r.page.URL, 90), r.page.LinkCount, r.page.EmailCount)
			}

			// 默认只抓「首页 + 优先路径」；开关打开后才跟进页面里的其他链接
			if r.item.depth >= c.cfg.Depth || !c.cfg.AllLinks {
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
	}
	if probeHit+probeMiss > 0 {
		fmt.Printf("  优先路径：命中 %d，未命中 %d\n", probeHit, probeMiss)
	}

	nPages, nLinks, nMail := c.countFor(root)
	mode := "仅首页+优先路径"
	if c.cfg.AllLinks {
		mode = "首页+优先路径+链接页"
	}
	if c.linkTruncated[root] {
		fmt.Printf("  站点 %s 完成[%s]：页面 %d，链接 %d（已达上限 %d，已截断），邮箱 %d\n",
			root, mode, nPages, nLinks, c.cfg.MaxLinksPerSite, nMail)
	} else {
		fmt.Printf("  站点 %s 完成[%s]：页面 %d，链接 %d，邮箱 %d\n", root, mode, nPages, nLinks, nMail)
	}
	return nil
}

// buildPriorityQueue 把默认/自定义的优先路径转成待探测队列。
func (c *Crawler) buildPriorityQueue(seed *url.URL, visited map[string]bool) []queueItem {
	var out []queueItem
	for _, p := range c.cfg.PriorityPaths {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		u := *seed
		u.Path = p
		u.RawQuery = ""
		u.Fragment = ""
		key := canonicalURL(u.String())
		if visited[key] {
			continue
		}
		visited[key] = true
		out = append(out, queueItem{url: u.String(), depth: 1, probe: true})
	}
	return out
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

	// 探测页失败（404/超时等）时不写入库，避免页面清单被大量 404 刷屏
	if !c.fetcher.Allowed(ctx, item.url) {
		pg.Err = "robots.txt 禁止抓取"
		if !item.probe {
			c.store(pg, nil, nil)
		}
		return res
	}

	fetchFn := c.fetcher.Get
	if item.probe {
		fetchFn = c.fetcher.Probe
	}
	resp, err := fetchFn(ctx, item.url)
	if err != nil {
		pg.Err = err.Error()
		if !item.probe {
			c.store(pg, nil, nil)
		}
		return res
	}
	pg.StatusCode = resp.StatusCode
	pg.FinalURL = resp.FinalURL

	if resp.StatusCode >= 400 {
		pg.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		if !item.probe {
			c.store(pg, nil, nil)
		}
		return res
	}
	ct := strings.ToLower(resp.ContentType)
	if ct != "" && !strings.Contains(ct, "html") && !strings.Contains(ct, "xml") {
		pg.Err = "非 HTML 内容：" + ct
		if !item.probe {
			c.store(pg, nil, nil)
		}
		return res
	}

	doc, derr := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if derr != nil {
		pg.Err = "HTML 解析失败: " + derr.Error()
		if !item.probe {
			c.store(pg, nil, nil)
		}
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
		if c.cfg.MaxLinksPerSite > 0 && c.linkCount[l.RootDomain] >= c.cfg.MaxLinksPerSite {
			c.linkTruncated[l.RootDomain] = true
			continue
		}
		c.linkCount[l.RootDomain]++
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

// ResetBlocked 清空受阻名单（补抓前调用，便于统计补抓后仍失败的站点）。
func (c *Crawler) ResetBlocked() {
	c.mu.Lock()
	c.blocked = nil
	c.mu.Unlock()
}

// BlockedSeeds 返回因持续 429 而未能抓取的站点清单。
func (c *Crawler) BlockedSeeds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.blocked...)
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
