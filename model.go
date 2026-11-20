package main

import (
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// SelectorRule 一条链接提取规则：某个 CSS 选择器 + 它归属的页面位置。
type SelectorRule struct {
	Selector string
	Location string
}

// DefaultSelectorRules 覆盖 Shopify 的常见主题结构：
//   - 新版主题（Online Store 2.0）用 <header id="shopify-section-sections--xxx__header">
//   - 老版主题用 <div id="shopify-section-header">
//
// 因此除了精确 id 选择器，也保留 header/footer/nav 语义标签兜底。
var DefaultSelectorRules = []SelectorRule{
	{`#shopify-section-header a[href]`, "header"},
	{`#shopify-section-footer a[href]`, "footer"},
	{`[id^="shopify-section-"][id*="header"] a[href]`, "header"},
	{`[id^="shopify-section-"][id*="footer"] a[href]`, "footer"},
	{`header a[href]`, "header"},
	{`[role="banner"] a[href]`, "header"},
	{`footer a[href]`, "footer"},
	{`[role="contentinfo"] a[href]`, "footer"},
	{`nav a[href]`, "nav"},
	{`[role="navigation"] a[href]`, "nav"},
}

// Link 一条从页面中提取出来的 a[href]。
type Link struct {
	RootDomain string // 目标站点的主域名
	SourcePage string // 该链接出现在哪个页面
	Location   string // header / footer / nav / body / custom
	Text       string // 锚文本
	RawHref    string // HTML 里的原始值
	URL        string // 补全后的绝对地址
	Host       string
	Scope      string // internal / subdomain / external
	Crawlable  bool   // 是否纳入抓取队列
	Crawled    bool   // 是否已作为页面被抓取
	Status     int    // HTTP 状态码（未校验为 0）
	Alive      string // 有效 / 无效 / 受限 / 未校验
	Note       string // 跳过原因等备注
	Order      int
}

// PageResult 单个页面的抓取结果。
type PageResult struct {
	RootDomain string
	URL        string
	FinalURL   string
	StatusCode int
	Title      string
	LinkCount  int
	EmailCount int
	Fetched    bool
	Err        string
}

// EmailHit 一条命中的邮箱。
type EmailHit struct {
	Domain     string // 所属站点主域名
	Email      string
	SourcePage string
	Method     string // text / mailto / cloudflare / attribute
	Type       string // 企业邮箱 / 免费邮箱 / 系统邮箱
}

// Snapshot 导出所需的全部结果。
type Snapshot struct {
	Domains    []string
	Pages      []*PageResult
	Links      []*Link
	Emails     []*EmailHit
	PageOK     int
	OutputPath string
}

// ---------------------------------------------------------------------------
// 域名 / URL 工具
// ---------------------------------------------------------------------------

// rootDomain 取 eTLD+1，例如 shop.example.co.uk -> example.co.uk
func rootDomain(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	// IP、localhost、内网单标签域名直接原样返回
	if net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return host
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	// 本地地址 / IP / 单标签域名
	return host
}

// siteKey 用于比较“是否同一个站点”：去掉 www. 前缀再比。
func siteKey(host string) string {
	host = strings.ToLower(host)
	return strings.TrimPrefix(host, "www.")
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// canonicalURL 归一化 URL 用于去重：抹掉协议、www、fragment、末尾斜杠与跟踪参数。
// 返回值刻意不带协议前缀，只作为去重键使用。
func canonicalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, ":80")
	host = strings.TrimSuffix(host, ":443")
	host = strings.TrimPrefix(host, "www.")

	p := u.Path
	if p == "" {
		p = "/"
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}

	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "gclid" || lk == "fbclid" ||
			lk == "variant" || lk == "ref" || lk == "_pos" || lk == "_sid" || lk == "_ss" {
			q.Del(k)
		}
	}

	key := host + p
	if enc := q.Encode(); enc != "" {
		key += "?" + enc
	}
	return strings.ToLower(key)
}
