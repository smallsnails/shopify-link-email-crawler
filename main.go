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
	"sort"
	"strings"
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
	flag.IntVar(&maxPages, "max-pages", 100, "每个站点最多抓取页面数（含优先路径探测，建议 100 以内）")
	var maxLinks int
	flag.IntVar(&maxLinks, "max-links", 1000, "每个站点保留在链接清单中的最大条数，0 表示不限制")
	var priorityFile string
	flag.StringVar(&priorityFile, "priority-file", "", "自定义优先探测路径文件（每行一个以 / 开头的路径）")
	var noPriority bool
	flag.BoolVar(&noPriority, "no-priority", false, "关闭优先路径探测，仅按链接抓取")
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

	for i, seed := range cfg.Seeds {
		fmt.Printf("\n[%d/%d] 开始抓取 %s\n", i+1, len(cfg.Seeds), seed)
		if err := cr.CrawlSite(ctx, seed); err != nil {
			fmt.Printf("  [警告] %v\n", err)
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
