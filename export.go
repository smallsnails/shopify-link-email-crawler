package main

import (
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

const (
	sheetEmails  = "邮箱汇总"
	sheetLinks   = "链接清单"
	sheetPages   = "页面清单"
	sheetSites   = "站点统计"
	sheetSummary = "抓取统计"

	brandColor = "#2F5597"
	headColor  = "#FFFFFF"
	lineColor  = "#D6DCE4"
)

type styles struct {
	header int
	text   int
	link   int
	title  int
	number int
}

// ExportExcel 把快照写成多工作表 Excel。
func ExportExcel(path string, s *Snapshot, cfg *Config) error {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	st, err := buildStyles(f)
	if err != nil {
		return err
	}

	if err := f.SetSheetName("Sheet1", sheetEmails); err != nil {
		return err
	}
	if _, err := f.NewSheet(sheetLinks); err != nil {
		return err
	}
	if _, err := f.NewSheet(sheetPages); err != nil {
		return err
	}
	if _, err := f.NewSheet(sheetSites); err != nil {
		return err
	}
	if _, err := f.NewSheet(sheetSummary); err != nil {
		return err
	}

	// ---------------- 邮箱汇总 ----------------
	{
		sh := sheetEmails
		head := []interface{}{"序号", "来源域名", "来源页面", "邮箱", "邮箱类型", "归属", "抓取方式"}
		writeHeader(f, sh, head, st.header)
		row := 2
		for i, e := range s.Emails {
			_ = f.SetSheetRow(sh, "A"+strconv.Itoa(row), &[]interface{}{
				i + 1, e.Domain, e.SourcePage, e.Email, e.Type, emailOwnership(e), methodLabel(e.Method),
			})
			_ = f.SetCellStyle(sh, "A"+strconv.Itoa(row), "G"+strconv.Itoa(row), st.text)
			_ = f.SetCellHyperLink(sh, "C"+strconv.Itoa(row), e.SourcePage, "External")
			_ = f.SetCellStyle(sh, "C"+strconv.Itoa(row), "C"+strconv.Itoa(row), st.link)
			row++
		}
		if len(s.Emails) == 0 {
			_ = f.SetSheetRow(sh, "A2", &[]interface{}{"-", "-", "-", "未发现有效邮箱", "-", "-", "-"})
		}
		freezeAndFilter(f, sh, "G", row-1)
		setWidths(f, sh, map[string]float64{"A": 6, "B": 26, "C": 60, "D": 34, "E": 12, "F": 12, "G": 12})
	}

	// ---------------- 链接清单 ----------------
	{
		sh := sheetLinks
		head := []interface{}{"序号", "来源域名", "来源页面", "位置", "锚文本", "href(原始值)", "绝对链接", "链接域名", "类型", "HTTP状态", "可用性", "是否抓取", "备注"}
		writeHeader(f, sh, head, st.header)

		row := 2
		dropped := 0
		n := 0
		for _, l := range s.Links {
			if cfg != nil && cfg.DropDead && l.Alive == "无效" {
				dropped++
				continue
			}
			n++
			_ = f.SetSheetRow(sh, "A"+strconv.Itoa(row), &[]interface{}{
				n, l.RootDomain, l.SourcePage, l.Location, l.Text, dashIfEmpty(l.RawHref), l.URL,
				l.Host, scopeLabel(l.Scope), statusCell(l.Status), l.Alive,
				boolLabel(l.Crawled), dashIfEmpty(l.Note),
			})
			_ = f.SetCellStyle(sh, "A"+strconv.Itoa(row), "M"+strconv.Itoa(row), st.text)
			_ = f.SetCellHyperLink(sh, "C"+strconv.Itoa(row), l.SourcePage, "External")
			_ = f.SetCellHyperLink(sh, "G"+strconv.Itoa(row), l.URL, "External")
			_ = f.SetCellStyle(sh, "C"+strconv.Itoa(row), "C"+strconv.Itoa(row), st.link)
			_ = f.SetCellStyle(sh, "G"+strconv.Itoa(row), "G"+strconv.Itoa(row), st.link)
			row++
		}
		if n == 0 {
			_ = f.SetSheetRow(sh, "A2", &[]interface{}{"-", "-", "-", "-", "未提取到有效链接", "-", "-", "-", "-", "-", "-", "-", "-"})
		}
		freezeAndFilter(f, sh, "M", row-1)
		setWidths(f, sh, map[string]float64{
			"A": 6, "B": 24, "C": 46, "D": 12, "E": 26, "F": 30, "G": 52,
			"H": 24, "I": 10, "J": 10, "K": 12, "L": 10, "M": 26,
		})
	}

	// ---------------- 页面清单 ----------------
	{
		sh := sheetPages
		head := []interface{}{"序号", "来源域名", "页面地址", "HTTP状态", "页面标题", "链接数", "邮箱数", "备注"}
		writeHeader(f, sh, head, st.header)
		row := 2
		for i, p := range s.Pages {
			note := p.Err
			if note == "" && p.FinalURL != "" && p.FinalURL != p.URL {
				note = "重定向至 " + p.FinalURL
			}
			_ = f.SetSheetRow(sh, "A"+strconv.Itoa(row), &[]interface{}{
				i + 1, p.RootDomain, p.URL, statusCell(p.StatusCode), p.Title, p.LinkCount, p.EmailCount, note,
			})
			_ = f.SetCellStyle(sh, "A"+strconv.Itoa(row), "H"+strconv.Itoa(row), st.text)
			_ = f.SetCellHyperLink(sh, "C"+strconv.Itoa(row), p.URL, "External")
			_ = f.SetCellStyle(sh, "C"+strconv.Itoa(row), "C"+strconv.Itoa(row), st.link)
			row++
		}
		freezeAndFilter(f, sh, "H", row-1)
		setWidths(f, sh, map[string]float64{"A": 6, "B": 24, "C": 56, "D": 10, "E": 40, "F": 8, "G": 8, "H": 34})
	}

	// ---------------- 站点统计 ----------------
	{
		sh := sheetSites
		head := []interface{}{"序号", "站点", "抓取页面", "链接数", "有效链接", "无效链接", "受限/异常", "未校验", "邮箱数", "企业邮箱", "免费邮箱", "系统邮箱"}
		writeHeader(f, sh, head, st.header)

		row := 2
		for i, d := range s.Domains {
			var pages, links, alive, dead, restricted, unknown, mails, corp, free, sys int
			for _, p := range s.Pages {
				if p.RootDomain == d && p.Fetched {
					pages++
				}
			}
			for _, l := range s.Links {
				if l.RootDomain != d {
					continue
				}
				links++
				switch {
				case l.Alive == "无效":
					dead++
				case l.Alive == "未校验":
					unknown++
				case strings.HasPrefix(l.Alive, "有效"):
					alive++
				default:
					restricted++
				}
			}
			for _, e := range s.Emails {
				if e.Domain != d {
					continue
				}
				mails++
				switch e.Type {
				case "企业邮箱":
					corp++
				case "免费邮箱":
					free++
				case "系统邮箱":
					sys++
				}
			}
			_ = f.SetSheetRow(sh, "A"+strconv.Itoa(row), &[]interface{}{
				i + 1, d, pages, links, alive, dead, restricted, unknown, mails, corp, free, sys,
			})
			_ = f.SetCellStyle(sh, "A"+strconv.Itoa(row), "L"+strconv.Itoa(row), st.text)
			row++
		}
		freezeAndFilter(f, sh, "L", row-1)
		setWidths(f, sh, map[string]float64{"A": 6, "B": 30, "C": 10, "D": 10, "E": 10, "F": 10, "G": 12, "H": 10, "I": 10, "J": 10, "K": 10, "L": 10})
	}

	// ---------------- 抓取统计 ----------------
	{
		sh := sheetSummary
		_ = f.SetCellValue(sh, "A1", "抓取概况")
		_ = f.MergeCell(sh, "A1", "B1")
		_ = f.SetCellStyle(sh, "A1", "B1", st.title)

		totalLinks, aliveLinks, deadLinks, otherLinks, crawledPages := 0, 0, 0, 0, 0
		for _, l := range s.Links {
			totalLinks++
			switch {
			case l.Alive == "无效":
				deadLinks++
			case l.Alive == "未校验":
			case strings.HasPrefix(l.Alive, "有效"):
				aliveLinks++
			default:
				otherLinks++
			}
		}
		for _, p := range s.Pages {
			if p.Fetched {
				crawledPages++
			}
		}
		corp := 0
		for _, e := range s.Emails {
			if e.Type == "企业邮箱" {
				corp++
			}
		}

		rows := [][]interface{}{
			{"指标", "数值"},
			{"站点数量", len(s.Domains)},
			{"成功抓取页面", crawledPages},
			{"链接总数", totalLinks},
			{"有效链接", aliveLinks},
			{"无效链接（已从清单剔除）", deadLinks},
			{"受限/异常链接", otherLinks},
			{"有效邮箱总数", len(s.Emails)},
			{"其中企业邮箱", corp},
			{"导出文件", s.OutputPath},
		}
		r := 3
		for i, rec := range rows {
			_ = f.SetSheetRow(sh, "A"+strconv.Itoa(r), &rec)
			if i == 0 {
				_ = f.SetCellStyle(sh, "A"+strconv.Itoa(r), "B"+strconv.Itoa(r), st.header)
			} else {
				_ = f.SetCellStyle(sh, "A"+strconv.Itoa(r), "B"+strconv.Itoa(r), st.text)
			}
			r++
		}
		setWidths(f, sh, map[string]float64{"A": 22, "B": 60})
	}

	if _, err := f.NewSheet("说明"); err == nil {
		sh := "说明"
		notes := [][]interface{}{
			{"字段/规则", "说明"},
			{"抓取范围", "首页 + header/footer/nav 中提取到的链接页（深度由 -depth 控制）"},
			{"位置", "header / footer / nav / body（body 表示按区块选择器未命中，退回整页扫描）"},
			{"已排除的链接", "空 href、纯锚点 #、mailto:/tel:/javascript:/data: 等伪协议、静态资源（图片/CSS/JS/字体/PDF 等）、cdn-cgi 邮件保护链接"},
			{"可用性", "有效=2xx/3xx，受限=401/403/429，无效=404/410 或请求失败，未校验=未探测"},
			{"邮箱有效性", "已过滤图片名误匹配、占位域名（example.com 等）、16 进制哈希、非法 TLD 与格式错误"},
			{"邮箱类型", "企业邮箱=自有域名；免费邮箱=gmail/qq/163 等公共邮箱；系统邮箱=noreply/postmaster 等"},
			{"建议", "抓取结果仅用于合规的商业联系用途，请遵守目标站点 robots.txt 与当地隐私法规"},
		}
		for i, rec := range notes {
			_ = f.SetSheetRow(sh, "A"+strconv.Itoa(i+1), &rec)
			if i == 0 {
				_ = f.SetCellStyle(sh, "A1", "B1", st.header)
			} else {
				_ = f.SetCellStyle(sh, "A"+strconv.Itoa(i+1), "B"+strconv.Itoa(i+1), st.text)
			}
		}
		setWidths(f, sh, map[string]float64{"A": 18, "B": 100})
	}

	f.SetActiveSheet(0)
	if err := f.SaveAs(path); err != nil {
		return err
	}
	return nil
}

func buildStyles(f *excelize.File) (*styles, error) {
	border := []excelize.Border{
		{Type: "left", Color: lineColor, Style: 1},
		{Type: "right", Color: lineColor, Style: 1},
		{Type: "top", Color: lineColor, Style: 1},
		{Type: "bottom", Color: lineColor, Style: 1},
	}
	header, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: headColor, Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{brandColor}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border:    border,
	})
	if err != nil {
		return nil, err
	}
	text, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: false},
		Border:    border,
	})
	if err != nil {
		return nil, err
	}
	link, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Color: "#0563C1", Underline: "single"},
		Alignment: &excelize.Alignment{Vertical: "center"},
		Border:    border,
	})
	if err != nil {
		return nil, err
	}
	title, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 13, Color: brandColor},
		Alignment: &excelize.Alignment{Horizontal: "left", Vertical: "center"},
	})
	if err != nil {
		return nil, err
	}
	return &styles{header: header, text: text, link: link, title: title}, nil
}

func writeHeader(f *excelize.File, sheet string, head []interface{}, styleID int) {
	_ = f.SetSheetRow(sheet, "A1", &head)
	last := columnName(len(head))
	_ = f.SetCellStyle(sheet, "A1", last+"1", styleID)
	_ = f.SetRowHeight(sheet, 1, 26)
}

func freezeAndFilter(f *excelize.File, sheet, lastCol string, lastRow int) {
	_ = f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	})
	if lastRow < 2 {
		return
	}
	_ = f.AutoFilter(sheet, "A1:"+lastCol+strconv.Itoa(lastRow), nil)
}

func setWidths(f *excelize.File, sheet string, widths map[string]float64) {
	for col, w := range widths {
		_ = f.SetColWidth(sheet, col, col, w)
	}
}

func columnName(n int) string {
	name, err := excelize.ColumnNumberToName(n)
	if err != nil {
		return "Z"
	}
	return name
}

// ---------------------------------------------------------------------------
// 展示辅助
// ---------------------------------------------------------------------------

func methodLabel(m string) string {
	switch m {
	case "mailto":
		return "mailto 链接"
	case "cloudflare":
		return "Cloudflare 解码"
	case "attribute":
		return "源码/属性"
	default:
		return "页面文本"
	}
}

func scopeLabel(s string) string {
	switch s {
	case "internal":
		return "站内"
	case "subdomain":
		return "子域名"
	default:
		return "外部"
	}
}

func boolLabel(b bool) string {
	if b {
		return "已抓取"
	}
	return "未抓取"
}

func statusCell(code int) interface{} {
	if code <= 0 {
		return "-"
	}
	return code
}

// URLOrDash 空字符串统一显示为 -。
// emailOwnership 判断邮箱是否属于店铺自身域名。
// 抓取中常见第三方邮箱（物流商、建站插件），标出来后可在 Excel 里一键筛掉。
func emailOwnership(e *EmailHit) string {
	i := strings.LastIndex(e.Email, "@")
	if i < 0 || i == len(e.Email)-1 {
		return "未知"
	}
	dom := strings.ToLower(e.Email[i+1:])
	if dom == e.Domain || strings.HasSuffix(dom, "."+e.Domain) {
		return "本店域名"
	}
	if siteKey(dom) == siteKey(e.Domain) {
		return "本店域名"
	}
	return "第三方"
}

func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
