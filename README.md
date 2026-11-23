# Shopify 链接与邮箱抓取器

按 Shopify 店铺域名批量抓取 header / footer / nav 中的 `a[href]` 链接，跟进这些链接页面，
提取有效邮箱，最终输出一份多工作表的 Excel。

纯 Go 实现，单二进制，无需浏览器。

---

## 一、需求对应关系

| 需求 | 实现位置 | 说明 |
| --- | --- | --- |
| 1. 抓 header/footer/nav 里的 `a[href]`，排除无效链接 | `extract.go` `ExtractLinks` | 伪协议、空锚点、静态资源、邮件保护链接全部剔除 |
| 2. 支持 `#shopify-section-header a` 这类选择器 | `model.go` `DefaultSelectorRules` | 内置 10 条 Shopify 常见主题选择器，可用 `-selectors` 追加 |
| 3. 抓首页 + 链接页面 | `crawl.go` `CrawlSite` | **先探测 24 条联系页优先路径**，再按深度 BFS 抓链接页，去重 + 同域串行限速 |
| 4. 提取邮箱并排除无效 | `email.go` | 11 类校验规则，含 Cloudflare 解码、`[at]` 混淆还原 |
| 5. 导出 Excel（链接 + 邮箱） | `export.go` | 5 个工作表：邮箱汇总 / 链接清单 / 页面清单 / 站点统计 / 抓取统计 |

---

## 二、编译与运行

```bash
# 编译（需要 Go 1.21+）
go mod tidy
go build -o shopify-crawler.exe .

# 单个店铺
./shopify-crawler.exe -u https://www.example.com -o result.xlsx

# 批量：域名清单
./shopify-crawler.exe -f domains.txt -o result.xlsx -c 6 --max-pages 60
```

`domains.txt` 每行一个域名或 URL，`#` 开头为注释：

```
# 北美客户
allbirds.com
https://www.brooklinen.com
gymshark.com
```

### 参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-u` | — | 店铺域名/网址，可重复 |
| `-f` | — | 域名清单文件 |
| `-o` | `result.xlsx` | 输出 Excel 路径 |
| `-c` | `6` | 并发数（同域名请求仍会串行限速） |
| `--max-pages` | `30` | 单站点最多抓取页面数（默认模式只需 25 个名额：首页 + 24 条优先路径） |
| `--max-links` | `100` | 单站点保留在「链接清单」中的最大条数，超出截断 |
| `--all-links` | 关闭 | **额外抓取页面里的其他链接页**（默认关闭，只抓首页+优先路径，速度快） |
| `--site-workers` | `3` | 同时抓取的站点数。不同站点是不同域名，各自独立限速互不等待；批量清单建议 3~6 |
| `--delay` | `500ms` | 同一域名两次请求的最小间隔。**注意这是单域名间隔不是全局间隔**，设成 `5s` 会让每站 25 个请求串行等 2 分钟 |
| `--depth` | `1` | 抓取深度，1 = 首页 + 首页链接页 |
| `--delay` | `500ms` | 同域名两次请求的最小间隔 |
| `--timeout` | `20s` | 单请求超时 |
| `-verify` | `true` | 是否校验链接可用性（HEAD） |
| `--verify-external` | `false` | 是否校验外站链接（默认关闭，避免风控） |
| `--max-verify` | `400` | 单站点最多校验的链接数 |
| `--drop-dead` | `true` | 链接清单中剔除 404/410 的死链 |
| `--subdomains` | `false` | 是否跟随同主域子域名（如 blog.xxx.com） |
| `--crawl-external` | `false` | 是否抓取外站链接 |
| `--robots` | `true` | 是否遵守 robots.txt |
| `--no-priority` | 关闭 | 关闭优先路径探测，只按链接抓取 |
| `--priority-file` | — | 追加自定义优先路径文件（每行一个以 `/` 开头的路径） |
| `-selectors` | — | 自定义选择器文件，每行 `位置=选择器` |
| `--ignore-domains` | 内置列表 | 需忽略的邮箱域名，逗号分隔 |

---

## 三、输出结构

导出的 Excel 含 5 个工作表，表头冻结、带筛选器、URL 可点击：

1. **邮箱汇总** — 序号、来源域名、来源页面、邮箱、邮箱类型、抓取方式
2. **链接清单** — 序号、来源域名、来源页面、位置、锚文本、href 原始值、绝对链接、链接域名、
   站内/外部、HTTP 状态、可用性、是否抓取、备注
3. **页面清单** — 每个被抓取页面的状态码、标题、链接数、邮箱数
4. **站点统计** — 按站点的页面数、链接数、有效/无效链接、邮箱分类数量
5. **抓取统计** — 总体指标 + 规则说明

---

## 四、过滤规则

### 抓取顺序（`crawl.go`）

1. **首页**
2. **优先路径探测**（24 条内置，★ 标记）：`/contact`、`/contact-us`、`/pages/contact(-us)`、
   `/about(-us)`、`/pages/about(-us)`、`/faq(s)`、`/help(-center)`、`/support`、`/company-profile`、
   `/privacy-policy`、`/terms-of-service`、`/refund-policy`、`/shipping-policy` 以及
   Shopify 固定的 `/policies/*` 四条 —— 覆盖 Shopify 与 WordPress/WooCommerce/Magento 等
   通用独立站的联系页布局
3. **header/footer/nav 链接页** —— 默认**不抓**！只把链接记入「链接清单」供筛选；
   需要深入抓取时加 `--all-links`（速度会明显变慢）

说明：

- 默认模式每个站点只请求约 25 个页面（首页 + 优先路径），几十秒即可完成一个站点
- **首页打不开的站点（DNS 挂/超时/拒连）会立即短路**：跳过该站全部优先路径探测，死域名只花 1 个请求的时间
- 优先路径探测请求**不重试**，超时即放弃，单个失败最多 20s
- 优先路径探测未命中（404 等）**不会写入「页面清单」**，只在控制台汇总为"未命中 N"
- 探测页跟随重定向（如 `/support` → `support.xxx.com`），重定向后的页面照常提取邮箱
- 优先探测占用 `--max-pages` 名额：`max-pages=100` 时约剩 75 个名额给链接页

### 链接（`extract.go`）

排除：

- 空 `href`、纯锚点 `#top`
- 伪协议：`mailto:` `tel:` `sms:` `javascript:` `data:` `ftp:` `whatsapp:` `skype:`
- 静态资源：`.png/.jpg/.webp/.svg/.css/.js/.woff/.pdf/.zip` 等 30+ 扩展名
- Cloudflare 邮件保护链接 `/cdn-cgi/l/email-protection#...`
- 重复链接（同一页面内按归一化 URL 去重，并合并 `header+footer` 位置标注）
- 归一化时清掉 `utm_*`、`gclid`、`fbclid`、`variant` 等跟踪参数

标记为「不抓取」但仍保留在清单中：`/cart`、`/checkout`、`/account/*`、`/search`、
`/policies/refund`、`/password` 等功能页。

### 邮箱（`email.go`）

必须通过：

- 格式：`local@domain.tld`，local ≤ 64 字符、无首尾点、无连续点
- **TLD 必须是 ICANN 注册的真实顶级域**（`publicsuffix` 校验）——
  这条能挡掉文本节点粘连产生的 `help@allbirds.comphone` 之类误匹配
- 域名标签不以 `-` 开头/结尾，TLD 为 2–24 位字母

排除：

- 文件扩展名误匹配：`logo@2x.png`、`hero@3x.webp`
- 占位域名：`example.com`、`domain.com`、`test.com`、`yourdomain.com`
- 第三方 SDK 噪声：`*.sentry.io`、`*.wixpress.com`
- 24 位以上十六进制哈希（构建产物）
- IP、`localhost`、`user@-bad.com` 等非法形式
- **Unicode 转义残留**：JSON-LD 里的 `\u003cmail@x.com\u003e` 不会拼出 `u003email@x.com` 假邮箱

识别能力：

- `mailto:` 链接（含多人、带 `?subject=`）
- **Cloudflare 邮箱保护**：自动解码 `data-cfemail` / `/cdn-cgi/l/email-protection#`
- 混淆写法：`support[at]shop[dot]com`、`(at)`、`{at}`
- JSON-LD、`meta`、`data-*` 属性中的邮箱
- 可见文本（按节点加分隔符，避免相邻文本被拼成一个词）

分类：企业邮箱 / 免费邮箱（gmail、qq、163…）/ 系统邮箱（noreply、postmaster…）。

---

## 五、Shopify 实战注意点

1. **429 限流**：Shopify 对部分路径（常见 `/collections/*`）按 IP/UA 直接返回 429，
   与请求频率无关。程序会冷却重试，仍失败则标记「受限」继续跑，不会卡住整个任务。
   若长期 429，降低 `--delay` 频率、换出口 IP，或改用其公开的 `/products.json`、
   `sitemap.xml` 作为链接来源。
2. **两种主题结构**：
   - 老主题 `<div id="shopify-section-header">`
   - 新主题（Online Store 2.0）`<header id="shopify-section-sections--123__header">`
   因此除了精确 id 选择器，还保留 `header a`、`footer a`、`nav a`、`[role=navigation] a` 兜底。
3. **区块识别失败**：某页提取到的链接少于 5 条时，自动退回整页 `a[href]` 扫描（位置标 `body`）。
4. **纯 JS 渲染的自定义主题**：本工具不发请求渲染 JS。这类站点建议先拿到渲染后的 HTML
   再喂给解析层，或改用支持 headless 浏览器的方案。
5. **邮箱藏在图片/字体里**：Shopify 客服邮箱多出现在 footer 文本或 `mailto:`，本工具均已覆盖；
   少数店铺把邮箱做成图片，需要 OCR，不在本工具范围内。

---

## 六、测试

```bash
go test ./...          # 单元测试：邮箱校验、链接过滤、URL 归一化、CF 解码
```

`testdata/site` 是一个模拟 Shopify 主题结构的本地站点，可做端到端验证：

```bash
cd testdata/site && python -m http.server 8099
./shopify-crawler.exe -u http://127.0.0.1:8099 -o out.xlsx
```

预期：优先路径命中 `/contact`、`/pages/contact-us`、`/pages/about`（★ 标记），
`test@example.com`、`banner@2x.png`、`help@shop.comphone` 均被过滤。
（需用 `python serve.py` 启动，普通 `http.server` 不支持无扩展名的 Clean URL）

---

## 七、合规提示

抓取结果请仅用于合规的商业联系用途：遵守目标站点 `robots.txt`（默认开启）、
控制请求频率（默认 2 请求/秒/域名）、遵守 GDPR / 个人信息保护法对商业邮件的约束，
不要用于群发垃圾邮件。
