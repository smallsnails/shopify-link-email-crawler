package main

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestNormalizeEmail(t *testing.T) {
	valid := map[string]string{
		"Press@Brooklinen.com":             "press@brooklinen.com",
		"team@fixtureshop.co.uk":           "team@fixtureshop.co.uk",
		" john.doe+tag@sub.shop.com ":      "john.doe+tag@sub.shop.com",
		"mailto:sales@shop.com?subject=hi": "sales@shop.com",
		"support＠shop.com":                 "support＠shop.com", // 全角 @ 不转换，仅检测不 panic
	}
	for in, want := range valid {
		got, ok := NormalizeEmail(in)
		if in == "support＠shop.com" {
			continue // 全角 @ 属于无效输入，单独在 invalid 中断言
		}
		if !ok || got != want {
			t.Errorf("NormalizeEmail(%q) = (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}

	invalid := []string{
		"",
		"help@allbirds.comphone", // 文本节点粘连导致的误匹配
		"banner@2x.png",
		"hero@3x.webp",
		"user@localhost",
		"user@-bad.com",
		"user@bad-.com",
		"a@b.c",
		"@shop.com",
		"user@@shop.com",
		"user name@shop.com",
		"support＠shop.com",
		"019b1c2d3e4f5a6b7c8d9e0f@gmail.com",
	}
	for _, in := range invalid {
		if got, ok := NormalizeEmail(in); ok {
			t.Errorf("NormalizeEmail(%q) 应为无效，却得到 %q", in, got)
		}
	}
}

func TestClassifyEmail(t *testing.T) {
	cases := map[string]string{
		"a@gmail.com":      "免费邮箱",
		"noreply@shop.com": "系统邮箱",
		"sales@shop.com":   "企业邮箱",
	}
	for in, want := range cases {
		if got := ClassifyEmail(in); got != want {
			t.Errorf("ClassifyEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeCFEmail(t *testing.T) {
	// "info@fixtureshop.com" 以 key=0x5a 异或编码
	if got := decodeCFEmail("5a33343c351a3c33222e2f283f2932352a74393537"); got != "info@fixtureshop.com" {
		t.Fatalf("decodeCFEmail = %q", got)
	}
}

func TestCanonicalURL(t *testing.T) {
	cases := map[string]string{
		"https://www.Shop.com/collections/all/?utm_source=x&variant=1": "shop.com/collections/all",
		"http://shop.com/":      "shop.com/",
		"https://shop.com/#top": "shop.com/",
	}
	for in, want := range cases {
		if got := canonicalURL(in); got != want {
			t.Errorf("canonicalURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractLinksFilters(t *testing.T) {
	html := `<!doctype html><html><body>
<header id="shopify-section-header"><nav>
  <a href="/pages/about">About</a>
  <a href="#top">Top</a>
  <a href="javascript:void(0)">JS</a>
  <a href="mailto:a@b.com">Mail</a>
  <a href="tel:+123">Call</a>
  <a href="/assets/logo@2x.png">Logo</a>
  <a href="/cart">Cart</a>
  <a href="https://other-site.com/x">Ext</a>
</nav></header>
<footer id="shopify-section-footer">
  <a href="/policies/privacy-policy">Privacy</a>
  <a href="/pages/about">About again</a>
</footer>
</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Rules: DefaultSelectorRules}
	links, excluded := ExtractLinks(doc, "https://shop.com/", "shop.com", cfg)

	byURL := map[string]*Link{}
	for _, l := range links {
		byURL[l.URL] = l
	}
	if _, ok := byURL["https://shop.com/pages/about"]; !ok {
		t.Fatalf("应提取到 /pages/about，实际 %d 条链接", len(links))
	}
	if _, ok := byURL["https://shop.com/cart"]; !ok {
		t.Error("购物车链接应保留在清单中（标记不可抓取）")
	} else if byURL["https://shop.com/cart"].Crawlable {
		t.Error("/cart 应标记为不可抓取")
	}
	for _, bad := range []string{"https://shop.com/assets/logo@2x.png"} {
		if _, ok := byURL[bad]; ok {
			t.Errorf("%s 不应出现在链接清单", bad)
		}
	}
	for _, l := range links {
		if strings.HasPrefix(l.URL, "mailto") || strings.HasPrefix(l.URL, "tel") || strings.Contains(l.URL, "#top") {
			t.Errorf("伪协议/锚点链接未被排除: %s", l.URL)
		}
	}
	if excluded < 4 {
		t.Errorf("排除计数偏低: %d", excluded)
	}
	// 同一链接在 header 与 footer 都出现时应合并位置标注
	if loc := byURL["https://shop.com/pages/about"].Location; !strings.Contains(loc, "header") || !strings.Contains(loc, "footer") {
		t.Errorf("位置合并失败: %q", loc)
	}
	// 外链应标记为 external 且不抓取
	if l := byURL["https://other-site.com/x"]; l == nil || l.Scope != "external" || l.Crawlable {
		t.Errorf("外链处理不正确: %+v", l)
	}
}

func TestUnicodeEscapeRemnant(t *testing.T) {
	// JSON 里常见 \u003cname@x.com\u003e，不应产生 u003e 开头的假邮箱
	html := `<script>var x = "support@gymshark.com\u003cbr\u003e";</script><p>contact@gymshark.com</p>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	hits := ExtractEmailsFromPage([]byte(html), doc, "https://www.gymshark.com/", "gymshark.com", nil)
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Email] = true
	}
	if !got["support@gymshark.com"] || !got["contact@gymshark.com"] {
		t.Errorf("应命中两个真实邮箱，实际: %v", got)
	}
	for e := range got {
		if strings.HasPrefix(e, "u00") {
			t.Errorf("不应出现 Unicode 转义残留邮箱: %s", e)
		}
	}
}

func TestExtractEmailsFromPage(t *testing.T) {
	html := `<html><body>
<header id="shopify-section-header"><nav><a href="mailto:hello@shop.com">Mail</a></nav></header>
<p>Wholesale: wholesale@shop.com</p>
<p>Obfuscated: support[at]shop[dot]com</p>
<p>Placeholder: test@example.com</p>
<p>Noise: banner@2x.png</p>
<p><span>help@shop.com</span><span>phone</span></p>
</body></html>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	ignore := parseIgnore(defaultIgnoreDomains)
	hits := ExtractEmailsFromPage([]byte(html), doc, "https://shop.com/", "shop.com", ignore)

	got := map[string]bool{}
	for _, h := range hits {
		got[h.Email] = true
	}
	for _, want := range []string{"hello@shop.com", "wholesale@shop.com", "support@shop.com", "help@shop.com"} {
		if !got[want] {
			t.Errorf("缺少邮箱 %s，实际：%v", want, got)
		}
	}
	for _, bad := range []string{"test@example.com", "banner@2x.png", "help@shop.comphone"} {
		if got[bad] {
			t.Errorf("不应出现 %s", bad)
		}
	}
}
