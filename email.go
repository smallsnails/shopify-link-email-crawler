package main

import (
	"encoding/hex"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// 邮箱基础正则：local@domain.tld，要求域名至少有一个点。
var emailRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._%+\-]{0,63}@[A-Za-z0-9](?:[A-Za-z0-9\-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]{0,61}[A-Za-z0-9])?)+`)

// mailto 链接
var mailtoRe = regexp.MustCompile(`(?i)mailto:([^"'\s<>\\]+)`)

// Cloudflare 邮件保护：<a href="/cdn-cgi/l/email-protection#xxxx" data-cfemail="xxxx">
var cfEmailRe = regexp.MustCompile(`(?i)data-cfemail=["']([0-9a-f]+)["']`)
var cfHrefRe = regexp.MustCompile(`(?i)/cdn-cgi/l/email-protection#([0-9a-f]+)`)

// 常见占位 / 无效邮箱域名（可在 --ignore-domains 中追加）
const defaultIgnoreDomains = "example.com,example.org,example.net,domain.com,email.com,test.com,yourdomain.com,your-domain.com,sentry.io,ingest.sentry.io,wixpress.com,shopify.com,sentry-next.wixpress.com,localhost"

// 明显不是邮箱的“顶级域”（多为图片/静态资源文件名被误匹配）
var badTLD = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "svg": true,
	"ico": true, "bmp": true, "avif": true, "tif": true, "tiff": true,
	"css": true, "js": true, "mjs": true, "json": true, "map": true,
	"woff": true, "woff2": true, "ttf": true, "otf": true, "eot": true,
	"mp4": true, "webm": true, "mov": true, "avi": true, "mp3": true, "wav": true,
	"pdf": true, "zip": true, "rar": true, "gz": true, "exe": true,
	"html": true, "htm": true, "php": true, "aspx": true, "jsp": true,
	"png2": true, "webp2": true,
}

// 免费邮箱域名
var freeMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "yahoo.com": true, "yahoo.co.jp": true,
	"hotmail.com": true, "outlook.com": true, "live.com": true, "msn.com": true,
	"aol.com": true, "icloud.com": true, "me.com": true, "mac.com": true,
	"qq.com": true, "foxmail.com": true, "163.com": true, "126.com": true,
	"yeah.net": true, "sina.com": true, "sina.cn": true, "sohu.com": true,
	"aliyun.com": true, "139.com": true, "189.cn": true, "21cn.com": true,
	"protonmail.com": true, "proton.me": true, "gmx.com": true, "gmx.de": true,
	"mail.ru": true, "yandex.com": true, "yandex.ru": true, "zoho.com": true,
	"hey.com": true, "fastmail.com": true, "tutanota.com": true, "web.de": true,
}

// 系统 / 无回复邮箱前缀
var systemLocalParts = map[string]bool{
	"noreply": true, "no-reply": true, "no_reply": true, "donotreply": true,
	"do-not-reply": true, "do_not_reply": true, "postmaster": true, "mailer-daemon": true,
	"abuse": true, "root": true, "hostmaster": true, "webmaster": true,
}

// deobfuscate 还原常见的邮箱混淆写法（只处理带括号的，避免误伤正常文本）。
var deobfuscator = strings.NewReplacer(
	"[at]", "@", "(at)", "@", "{at}", "@", "&#64;", "@", "＠", "@",
	"[dot]", ".", "(dot)", ".", "{dot}", ".", "&#46;", ".",
	"[AT]", "@", "(AT)", "@", "[DOT]", ".", "(DOT)", ".",
)

func deobfuscate(s string) string {
	if !strings.ContainsAny(s, "[(]{") {
		return s
	}
	return deobfuscator.Replace(s)
}

// NormalizeEmail 清洗并校验邮箱，返回 (规范化邮箱, 是否有效)。
func NormalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	e = strings.Trim(e, `.,;:!?)]}>"'\`+"`"+`，。、；：`)
	e = strings.TrimPrefix(e, "mailto:")
	if i := strings.IndexAny(e, "?&"); i > 0 {
		e = e[:i]
	}
	if strings.Contains(e, "%40") {
		e = strings.ReplaceAll(e, "%40", "@")
	}
	if e == "" || len(e) > 254 || strings.Count(e, "@") != 1 {
		return "", false
	}

	local, domain, _ := strings.Cut(e, "@")
	if local == "" || len(local) > 64 || domain == "" {
		return "", false
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", false
	}
	if strings.ContainsAny(local, " \t\r\n/\\") || strings.ContainsAny(domain, " \t\r\n/\\") {
		return "", false
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", false
	}
	tld := labels[len(labels)-1]
	if badTLD[tld] || len(tld) < 2 || len(tld) > 24 {
		return "", false
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || strings.HasPrefix(l, "-") || strings.HasSuffix(l, "-") {
			return "", false
		}
	}
	// 纯数字 TLD 基本是 IP 或误匹配
	if ok, _ := regexp.MatchString(`^\d+$`, tld); ok {
		return "", false
	}
	// 顶级域必须是 ICANN 注册的真实 TLD。
	// 这一条能挡掉文本节点拼接产生的 "help@allbirds.comphone" 之类误匹配。
	if ps, icann := publicsuffix.PublicSuffix(domain); ps == "" || !icann {
		return "", false
	}
	// 十六进制哈希样式（sentry / 构建产物）
	if len(local) >= 24 && isHex(local) {
		return "", false
	}
	return e, true
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ClassifyEmail 归类邮箱用途。
func ClassifyEmail(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	local, _, _ := strings.Cut(email, "@")
	if freeMailDomains[domain] {
		return "免费邮箱"
	}
	if systemLocalParts[local] {
		return "系统邮箱"
	}
	return "企业邮箱"
}

// decodeCFEmail 解出 Cloudflare 保护的邮箱。
func decodeCFEmail(s string) string {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) < 2 {
		return ""
	}
	key := b[0]
	out := make([]byte, len(b)-1)
	for i := 1; i < len(b); i++ {
		out[i-1] = b[i] ^ key
	}
	return string(out)
}

// ExtractEmailsFromText 从纯文本中提取邮箱。
func ExtractEmailsFromText(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range emailRe.FindAllString(deobfuscate(text), -1) {
		if e, ok := NormalizeEmail(m); ok && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// extractEmailCandidate 从原始 HTML 里找 mailto / Cloudflare 混淆邮箱。
func extractEmailCandidates(rawHTML string) []struct {
	Email  string
	Method string
} {
	var out []struct {
		Email  string
		Method string
	}
	seen := map[string]bool{}
	push := func(e, method string) {
		if e == "" || seen[e] {
			return
		}
		if normalized, ok := NormalizeEmail(e); ok {
			seen[normalized] = true
			out = append(out, struct {
				Email  string
				Method string
			}{normalized, method})
		}
	}

	for _, m := range mailtoRe.FindAllStringSubmatch(rawHTML, -1) {
		// mailto 可能是多人：mailto:a@x.com,b@y.com?subject=...
		target := m[1]
		if i := strings.IndexAny(target, "?"); i >= 0 {
			target = target[:i]
		}
		for _, part := range strings.Split(target, ",") {
			push(part, "mailto")
		}
	}
	for _, m := range cfEmailRe.FindAllStringSubmatch(rawHTML, -1) {
		push(decodeCFEmail(m[1]), "cloudflare")
	}
	for _, m := range cfHrefRe.FindAllStringSubmatch(rawHTML, -1) {
		push(decodeCFEmail(m[1]), "cloudflare")
	}
	return out
}

// sortEmails 稳定排序，便于输出可读。
func sortEmails(list []*EmailHit) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Domain != list[j].Domain {
			return list[i].Domain < list[j].Domain
		}
		return list[i].Email < list[j].Email
	})
}
