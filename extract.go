package main

import (
	"net/url"
	"path"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// 不抓取（但可能记录）的路径特征
var skipPathPrefixes = []string{
	"/cart", "/checkout", "/account/logout", "/account/login", "/orders",
	"/cdn-cgi/", "/admin", "/password", "/search", "/policies/refund",
	"/account", "/customer_authentication", "/challenge",
}

// 视为静态资源、不作为页面链接的扩展名
var assetExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".svg": true,
	".ico": true, ".bmp": true, ".avif": true, ".tif": true, ".tiff": true,
	".css": true, ".js": true, ".mjs": true, ".json": true, ".map": true, ".xml": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp4": true, ".webm": true, ".mov": true, ".mp3": true, ".wav": true, ".m4v": true,
	".zip": true, ".rar": true, ".gz": true, ".pdf": true, ".doc": true, ".docx": true,
}

// ExtractLinks 从已解析的文档里按规则提取 a[href]。
// 返回：有效链接列表、被排除的链接数。
func ExtractLinks(doc *goquery.Document, pageURL, root string, cfg *Config) ([]*Link, int) {
	base := pageURL
	if href, ok := doc.Find("base[href]").First().Attr("href"); ok {
		if b, err := url.Parse(pageURL); err == nil {
			if abs, err2 := b.Parse(href); err2 == nil {
				base = abs.String()
			}
		}
	}

	seen := map[string]int{} // canonical -> index in out
	var out []*Link
	excluded := 0
	order := 0

	add := func(sel *goquery.Selection, location string) {
		href, ok := sel.Attr("href")
		if !ok {
			return
		}
		order++
		link, reason, skip := buildLink(href, pageURL, base, root, location, cfg)
		if skip {
			excluded++
			return
		}
		link.Text = cleanAnchorText(sel)
		link.Order = order
		link.Note = reason

		key := canonicalURL(link.URL)
		if idx, dup := seen[key]; dup {
			// 同一链接出现在多个区块时合并位置标注
			if !strings.Contains(out[idx].Location, location) {
				out[idx].Location += "+" + location
			}
			if out[idx].Text == "" && link.Text != "" {
				out[idx].Text = link.Text
			}
			return
		}
		seen[key] = len(out)
		out = append(out, link)
	}

	// 1) 先按 header / footer / nav 规则提取
	for _, rule := range cfg.Rules {
		doc.Find(rule.Selector).Each(func(_ int, s *goquery.Selection) {
			add(s, rule.Location)
		})
	}

	// 2) 兜底：区块识别失败（例如纯 JS 渲染的自定义主题），退回整页扫描
	if len(out) < 5 {
		doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
			add(s, "body")
		})
	}

	return out, excluded
}

// buildLink 判断单个 href 是否有效，并补全为绝对地址。
func buildLink(href, pageURL, base, root string, location string, cfg *Config) (*Link, string, bool) {
	raw := strings.TrimSpace(href)
	lower := strings.ToLower(raw)

	if raw == "" || lower == "#" || strings.HasPrefix(lower, "#") {
		return nil, "", true
	}
	// 非 http(s) 协议一律排除（mailto 的邮箱由邮箱提取模块单独处理）
	for _, p := range []string{"mailto:", "tel:", "sms:", "javascript:", "data:", "ftp:", "whatsapp:", "skype:", "viber:", "intent:", "file:"} {
		if strings.HasPrefix(lower, p) {
			return nil, "", true
		}
	}
	if strings.HasPrefix(lower, "/cdn-cgi/l/email-protection") {
		return nil, "", true
	}

	b, err := url.Parse(base)
	if err != nil {
		return nil, "", true
	}
	abs, err := b.Parse(raw)
	if err != nil || abs.Host == "" {
		return nil, "", true
	}
	abs.Fragment = ""
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return nil, "", true
	}
	// 去掉跟踪参数
	q := abs.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "utm_") || lk == "gclid" || lk == "fbclid" ||
			lk == "variant" || lk == "ref" || lk == "_pos" || lk == "_sid" || lk == "_ss" {
			q.Del(k)
		}
	}
	abs.RawQuery = q.Encode()
	absURL := abs.String()

	// 超长 query 基本是鉴权 token / 分享追踪串，抓了也没意义
	if len(abs.RawQuery) > 300 {
		return nil, "超长参数链接", true
	}
	if strings.Contains(lower, "buyer_flags=") || strings.Contains(lower, "customer_authentication") {
		return nil, "鉴权跳转链接", true
	}

	// 静态资源
	ext := strings.ToLower(path.Ext(abs.Path))
	if assetExt[ext] {
		return nil, "静态资源", true
	}
	// 邮件保护链接
	if strings.Contains(strings.ToLower(abs.Path), "/cdn-cgi/l/email-protection") {
		return nil, "", true
	}

	host := strings.ToLower(abs.Hostname())
	targetRoot := rootDomain(host)

	pageHost := hostOf(pageURL)
	scope := "external"
	switch {
	case host == pageHost:
		scope = "internal"
	case targetRoot != "" && targetRoot == root:
		scope = "subdomain"
	}

	link := &Link{
		RootDomain: root,
		SourcePage: pageURL,
		Location:   location,
		RawHref:    raw,
		URL:        absURL,
		Host:       host,
		Scope:      scope,
		Alive:      "未校验",
	}

	note := ""
	switch {
	case scope == "external" && !cfg.CrawlExternal:
		note = "外部域名，不抓取"
	case scope == "subdomain" && !cfg.FollowSub:
		note = "子域名，未开启 -subdomains，不抓取"
	}

	// 购物流程页等不作为内容页抓取，但保留在链接清单里
	lp := strings.ToLower(abs.Path)
	for _, p := range skipPathPrefixes {
		if strings.HasPrefix(lp, p) {
			link.Crawlable = false
			if note == "" {
				note = "功能页，不抓取"
			}
			link.Note = note
			return link, note, false
		}
	}

	link.Crawlable = note == ""
	link.Note = note
	return link, note, false
}

func cleanAnchorText(s *goquery.Selection) string {
	t := strings.Join(strings.Fields(s.Text()), " ")
	if t == "" {
		if label, ok := s.Attr("aria-label"); ok {
			t = strings.Join(strings.Fields(label), " ")
		}
	}
	if t == "" {
		if title, ok := s.Attr("title"); ok {
			t = strings.Join(strings.Fields(title), " ")
		}
	}
	if len(t) > 120 {
		t = t[:120] + "..."
	}
	return t
}

// ExtractEmailsFromPage 提取一个页面里的全部候选邮箱。
func ExtractEmailsFromPage(rawHTML []byte, doc *goquery.Document, pageURL, domain string, ignore map[string]bool) []*EmailHit {
	if ignore == nil {
		ignore = map[string]bool{}
	}
	var hits []*EmailHit
	seen := map[string]bool{}

	push := func(email, method string) {
		e, ok := NormalizeEmail(email)
		if !ok || seen[e] {
			return
		}
		_, host, _ := strings.Cut(e, "@")
		if ignore[host] || strings.HasPrefix(host, "sentry.") || strings.Contains(host, ".wixpress.com") {
			return
		}
		seen[e] = true
		hits = append(hits, &EmailHit{
			Domain:     domain,
			Email:      e,
			SourcePage: pageURL,
			Method:     method,
			Type:       ClassifyEmail(e),
		})
	}

	raw := string(rawHTML)
	// 1) mailto / Cloudflare 混淆
	for _, c := range extractEmailCandidates(raw) {
		push(c.Email, c.Method)
	}
	// 2) 原始 HTML 全文（能覆盖 hidden input、JSON-LD、data-* 属性）
	for _, e := range ExtractEmailsFromText(raw) {
		push(e, "attribute")
	}
	// 3) 页面可见文本（按节点加入分隔符，避免相邻文本被拼成一个词）
	if doc != nil {
		if node := doc.Get(0); node != nil {
			for _, e := range ExtractEmailsFromText(visibleText(node)) {
				push(e, "text")
			}
		}
	}
	return hits
}

// visibleText 递归取文本，并在每个元素/文本节点之间插入空格。
// goquery 的 Text() 是直接拼接，会把 "<span>a@b.com</span><span>phone</span>"
// 变成 "a@b.comphone"，导致误匹配。
func visibleText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		switch node.Type {
		case html.TextNode:
			b.WriteString(node.Data)
			b.WriteByte(' ')
			return
		case html.ElementNode:
			switch node.Data {
			case "style", "noscript":
				return
			}
			for c := node.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			b.WriteByte(' ')
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
