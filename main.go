// Command shopify-crawler 抓取 Shopify 店铺（或任意站点）的 header / footer / nav
// 区块里的 a[href] 链接，抓取首页及这些链接页面，提取有效邮箱，最终导出 Excel。
//
// 用法示例：
//
//	shopify-crawler -u https://www.example.com -o result.xlsx
//	shopify-crawler -f domains.txt -o result.xlsx -c 8 --max-pages 80
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config 汇总一次抓取任务的运行参数。
type Config struct {
	Seeds          []string
	Output         string
	Workers        int
	MaxPages       int
	Depth          int
	Verify         bool
	Timeout        time.Duration
	Delay          time.Duration
	FollowSub      bool
	CrawlExternal  bool
	Robots         bool
	UA             string
	Rules          []SelectorRule
	IgnoreDomains  map[string]bool
	MaxLinksVerify int
	DropDead       bool
	VerifyExternal bool
	// 每个站点保留在链接清单中的最大条数，0 表示不限制
	MaxLinksPerSite int
	// 优先探测的联系页路径；nil 表示关闭该功能
	PriorityPaths []string
	// 是否额外抓取页面里的其他链接页（默认 false，只抓首页+优先路径）
	AllLinks bool
	// 所有域名共享的全局最小请求间隔，限制单 IP 总速率（Shopify/Cloudflare 走 IP 级限流）
	GlobalDelay time.Duration
	// 批次结束后是否对持续 429 的站点冷却补抓一轮
	RetryBlocked bool
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n[错误] %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var seeds multiFlag
	flag.Var(&seeds, "u", "店铺域名或网址，可重复：-u a.com -u b.com")
	var domainFile string
	flag.StringVar(&domainFile, "f", "", "域名清单文件（每行一个域名或 URL，# 开头为注释）")
	var output string
	flag.StringVar(&output, "o", "result.xlsx", "导出的 Excel 路径")
	var workers int
	flag.IntVar(&workers, "c", 6, "页面并发数")
	var maxPages int
	flag.IntVar(&maxPages, "max-pages", 30, "每个站点最多抓取页面数（默认模式 25 个名额即够：首页+24 条优先路径）")
	var maxLinks int
	flag.IntVar(&maxLinks, "max-links", 100, "每个站点保留在链接清单中的最大条数，0 表示不限制")
	var priorityFile string
	flag.StringVar(&priorityFile, "priority-file", "", "自定义优先探测路径文件（每行一个以 / 开头的路径）")
	var noPriority bool
	flag.BoolVar(&noPriority, "no-priority", false, "关闭优先路径探测，仅按链接抓取")
	var allLinks bool
	flag.BoolVar(&allLinks, "all-links", false, "额外抓取页面里的其他链接页（默认关闭，只抓首页+优先路径，速度快）")
	var siteWorkers int
	flag.IntVar(&siteWorkers, "site-workers", 3, "同时抓取的站点数（不同站点是不同域名，各自独立限速，互不等待）")
	var globalDelay time.Duration
	flag.DurationVar(&globalDelay, "global-delay", 2*time.Second, "所有域名共享的全局最小请求间隔（单 IP 总速率封顶 = 1/此值）。默认 2s = 0.5 请求/秒，最保守；跑得顺可降回 1s 或 500ms 提速")
	var retryBlocked bool
	flag.BoolVar(&retryBlocked, "retry-blocked", true, "批次结束后冷却 90s，对持续 429 的站点自动补抓一轮")
	var depth int
	flag.IntVar(&depth, "depth", 1, "抓取深度（1=首页+首页里的链接页）")
	var verify bool
	flag.BoolVar(&verify, "verify", true, "是否校验链接可用性（HEAD/GET）")
	var maxVerify int
	flag.IntVar(&maxVerify, "max-verify", 400, "每个站点最多校验的链接数，0 表示不限制")
	var timeout time.Duration
	flag.DurationVar(&timeout, "timeout", 20*time.Second, "单次请求超时")
	var delay time.Duration
	flag.DurationVar(&delay, "delay", 500*time.Millisecond, "同一域名两次请求的最小间隔")
	var followSub bool
	flag.BoolVar(&followSub, "subdomains", false, "是否跟随同主域的子域名（如 blog.xxx.com）")
	var crawlExternal bool
	flag.BoolVar(&crawlExternal, "crawl-external", false, "是否抓取外部域名的链接页面")
	var robots bool
	flag.BoolVar(&robots, "robots", true, "是否遵守 robots.txt")
	var selFile string
	flag.StringVar(&selFile, "selectors", "", "自定义 CSS 选择器文件，每行一个；可用 '位置=选择器' 指定归属")
	var ua string
	flag.StringVar(&ua, "ua", defaultUA, "User-Agent")
	var ignore string
	flag.StringVar(&ignore, "ignore-domains", defaultIgnoreDomains, "需要忽略的邮箱域名，逗号分隔")
	var verifyExternal bool
	flag.BoolVar(&verifyExternal, "verify-external", false, "是否校验外部域名链接的可用性（默认关闭，避免触发风控）")
	var dropDead bool
	flag.BoolVar(&dropDead, "drop-dead", true, "链接清单中剔除校验为无效(404/410)的链接")
	flag.Parse()

	// ---- 优先探测路径 ----
	var priority []string
	if !noPriority {
		priority = append(priority, DefaultPriorityPaths...)
		if priorityFile != "" {
			lines, err := readLines(priorityFile)
			if err != nil {
				return fmt.Errorf("读取优先路径文件失败: %w", err)
			}
			priority = append(priority, lines...)
		}
		// 去重并过滤非法项
		pSeen := map[string]bool{}
		var clean []string
		for _, p := range priority {
			p = strings.TrimSpace(p)
			if p == "" || !strings.HasPrefix(p, "/") || pSeen[p] {
				continue
			}
			pSeen[p] = true
			clean = append(clean, p)
		}
		priority = clean
	}

	// ---- 收集入口域名 ----
	all := append([]string{}, seeds...)
	if domainFile != "" {
		lines, err := readLines(domainFile)
		if err != nil {
			return fmt.Errorf("读取域名文件失败: %w", err)
		}
		all = append(all, lines...)
	}
	if len(all) == 0 {
		flag.Usage()
		return errors.New("请至少提供一个店铺地址：-u https://xxx.com 或 -f domains.txt")
	}
	if workers < 1 {
		workers = 1
	}
	if workers > 16 {
		workers = 16
	}
	if depth < 0 {
		depth = 0
	}
	if siteWorkers < 1 {
		siteWorkers = 1
	}
	if siteWorkers > 8 {
		siteWorkers = 8
	}

	rules := append([]SelectorRule{}, DefaultSelectorRules...)
	if selFile != "" {
		lines, err := readLines(selFile)
		if err != nil {
			return fmt.Errorf("读取选择器文件失败: %w", err)
		}
		for _, line := range lines {
			loc, sel := "custom", line
			if i := strings.Index(line, "="); i > 0 {
				loc, sel = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
			}
			if sel != "" {
				rules = append([]SelectorRule{{Selector: sel, Location: loc}}, rules...)
			}
		}
	}

	cfg := &Config{
		Output:          output,
		Workers:         workers,
		MaxPages:        maxPages,
		Depth:           depth,
		Verify:          verify,
		Timeout:         timeout,
		Delay:           delay,
		FollowSub:       followSub,
		CrawlExternal:   crawlExternal,
		Robots:          robots,
		UA:              ua,
		Rules:           rules,
		IgnoreDomains:   parseIgnore(ignore),
		MaxLinksVerify:  maxVerify,
		DropDead:        dropDead,
		VerifyExternal:  verifyExternal,
		MaxLinksPerSite: maxLinks,
		PriorityPaths:   priority,
		AllLinks:        allLinks,
		GlobalDelay:     globalDelay,
		RetryBlocked:    retryBlocked,
	}

	// ---- 规范化入口 ----
	seen := map[string]bool{}
	for _, raw := range all {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		u, err := normalizeSeed(raw)
		if err != nil {
			fmt.Printf("[跳过] %s -> %v\n", raw, err)
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		cfg.Seeds = append(cfg.Seeds, u)
	}
	if len(cfg.Seeds) == 0 {
		return errors.New("没有解析出有效的店铺地址")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cr := NewCrawler(cfg)
	start := time.Now()

	// 站点级并行：不同站点是不同域名，各自走独立限速队列，互不阻塞。
	// 站点内部仍按 delay 对同一域名串行，礼貌性不受影响。
	sem := make(chan struct{}, siteWorkers)
	var siteWG sync.WaitGroup
	for i, seed := range cfg.Seeds {
		if ctx.Err() != nil {
			fmt.Printf("\n[中止] 已停止，剩余 %d 个站点未抓取\n", len(cfg.Seeds)-i)
			break
		}
		siteWG.Add(1)
		sem <- struct{}{}
		go func(i int, seed string) {
			defer siteWG.Done()
			defer func() { <-sem }()
			fmt.Printf("\n[%d/%d] 开始抓取 %s\n", i+1, len(cfg.Seeds), seed)
			if err := cr.CrawlSite(ctx, seed); err != nil {
				fmt.Printf("  [警告] %v\n", err)
			}
		}(i, seed)
	}
	siteWG.Wait()

	// 持续 429 的站点：等 IP 冷却后再补抓一轮（重试期间它们不再刷新惩罚）
	if cfg.RetryBlocked {
		if blocked := cr.BlockedSeeds(); len(blocked) > 0 && ctx.Err() == nil {
			fmt.Printf("\n[补抓] %d 个站点因持续 429 被拒，冷却 90s 后重试 ...\n", len(blocked))
			select {
			case <-ctx.Done():
				fmt.Printf("[中止] 补抓取消\n")
			case <-time.After(90 * time.Second):
				cr.ResetBlocked()
				for _, seed := range blocked {
					if ctx.Err() != nil {
						break
					}
					fmt.Printf("\n[补抓] 开始抓取 %s\n", seed)
					if err := cr.CrawlSite(ctx, seed); err != nil {
						fmt.Printf("  [警告] %v\n", err)
					}
				}
			}
		}
	}

	// 补抓后仍被 429 挡住（或干脆拒绝访问）的站点，写成清单方便换个时间重跑
	if failed := cr.BlockedSeeds(); len(failed) > 0 {
		path := retryListPath(cfg.Output)
		if err := writeLines(path, failed); err != nil {
			fmt.Printf("[提示] %d 个站点仍被 429 拒绝，但写入 %s 失败: %v\n", len(failed), path, err)
		} else {
			fmt.Printf("\n[提示] %d 个站点仍被 429 拒绝，已写入 %s\n", len(failed), path)
			fmt.Printf("       稍后（建议隔 1 小时以上）用 -f %s 单独重跑这几站\n", path)
		}
	}

	if cfg.Verify {
		fmt.Printf("\n[校验] 正在检测链接可用性 ...\n")
		cr.VerifyLinks(ctx)
	}

	fmt.Printf("\n[导出] 写入 Excel: %s\n", cfg.Output)
	snap := cr.Snapshot()
	snap.OutputPath = cfg.Output
	if err := ExportExcel(cfg.Output, snap, cfg); err != nil {
		return fmt.Errorf("导出 Excel 失败: %w", err)
	}

	printConsoleSummary(snap, time.Since(start))
	return nil
}

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// writeLines 把字符串写入文件，每行一个。
func writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	var b strings.Builder
	b.WriteString("# 以下站点因持续 HTTP 429 未能抓取，建议隔 1 小时以上单独重跑\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// retryListPath 由输出文件名派生失败清单路径：output/x.xlsx -> output/x-429.txt
func retryListPath(output string) string {
	dir := filepath.Dir(output)
	base := filepath.Base(output)
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return filepath.Join(dir, base+"-429.txt")
}

// normalizeSeed 把用户输入补全为完整 URL。
func normalizeSeed(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'")
	if s == "" {
		return "", errors.New("空地址")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("无法解析主机名: %s", s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("不支持的协议: %s", u.Scheme)
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func parseIgnore(s string) map[string]bool {
	m := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			m[part] = true
		}
	}
	return m
}

func printConsoleSummary(s *Snapshot, cost time.Duration) {
	fmt.Printf("\n================ 抓取结果 ================\n")
	fmt.Printf("耗时: %s\n", cost.Round(time.Millisecond))
	fmt.Printf("站点: %d 个 | 成功页面: %d | 链接: %d | 有效邮箱: %d\n",
		len(s.Domains), s.PageOK, len(s.Links), len(s.Emails))

	for _, d := range s.Domains {
		fmt.Printf("\n● %s\n", d)
		nLink, nMail := 0, 0
		for _, e := range s.Emails {
			if e.Domain == d {
				nMail++
			}
		}
		for _, l := range s.Links {
			if l.RootDomain == d {
				nLink++
			}
		}
		fmt.Printf("  链接 %d 条，邮箱 %d 个\n", nLink, nMail)

		// 每个站点列出前 10 个邮箱
		var list []string
		for _, e := range s.Emails {
			if e.Domain == d {
				list = append(list, e.Email+"  <"+shorten(e.SourcePage, 60)+">")
			}
		}
		sort.Strings(list)
		for i, item := range list {
			if i >= 10 {
				fmt.Printf("  ... 其余 %d 个见 Excel\n", len(list)-10)
				break
			}
			fmt.Printf("  - %s\n", item)
		}
	}
	fmt.Printf("\n明细见 Excel：%s\n", s.OutputPath)
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
