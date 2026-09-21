package utils

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	nethtml "golang.org/x/net/html"
)

const maxRecursionDepth = 256

var (
	// Tailwind & CSS 响应式多列检测正则
	tailwindGridRegex = regexp.MustCompile(`(?:\b[a-z0-9]+:)?grid-cols-(\d+)\b`)
	tailwindColsRegex = regexp.MustCompile(`(?:\b[a-z0-9]+:)?columns-(\d+)\b`)
	cssGridRepRegex   = regexp.MustCompile(`grid-template-columns:\s*repeat\((\d+)`)
	cssColCountRegex  = regexp.MustCompile(`column-count:\s*(\d+)`)

	// 语言检测多格式支持（Prism, Highlight.js, Shiki, GitHub 等）
	langRegex         = regexp.MustCompile(`(?:language|lang)-([a-zA-Z0-9_\-\+]+)`)
	ghSourceLangRegex = regexp.MustCompile(`\bhighlight-source-([a-zA-Z0-9_\-\+]+)\b`)

	// 标题锚点匹配
	headingAnchorRegex = regexp.MustCompile(`(?i)\b(?:anchor|header-anchor|hash-link|permalink|section-link)\b`)

	// XSS 与控制字符安全正则
	ctrlCharsRegex = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	safeSchemes    = map[string]bool{
		"http":   true,
		"https":  true,
		"mailto": true,
		"tel":    true,
	}
)

// -------------------------------------------------------------
// 缓冲管理模块
// -------------------------------------------------------------

type mdBuffer struct {
	buf              []byte
	trailingNewlines int
}

func newMdBuffer() *mdBuffer {
	return &mdBuffer{buf: make([]byte, 0, 1024)}
}

func (b *mdBuffer) WriteString(s string) {
	if len(s) == 0 {
		return
	}
	b.buf = append(b.buf, s...)
	b.trailingNewlines = 0
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			b.trailingNewlines++
		} else {
			break
		}
	}
}

// WriteByte appends a single byte and returns nil. The error return keeps the
// signature compatible with the io.ByteWriter convention (go vet flags a
// WriteByte method without it).
func (b *mdBuffer) WriteByte(c byte) error {
	b.buf = append(b.buf, c)
	if c == '\n' {
		b.trailingNewlines++
	} else {
		b.trailingNewlines = 0
	}
	return nil
}

func (b *mdBuffer) LastByte() byte {
	if len(b.buf) == 0 {
		return 0
	}
	return b.buf[len(b.buf)-1]
}

func (b *mdBuffer) TrimTrailingSpaces() {
	for len(b.buf) > 0 && (b.buf[len(b.buf)-1] == ' ' || b.buf[len(b.buf)-1] == '\t') {
		b.buf = b.buf[:len(b.buf)-1]
	}
}

func (b *mdBuffer) EnsureNewlines(n int) {
	if len(b.buf) == 0 || n <= 0 {
		return
	}
	b.TrimTrailingSpaces()
	for b.trailingNewlines < n {
		_ = b.WriteByte('\n')
	}
}

func (b *mdBuffer) String() string {
	return string(b.buf)
}

// -------------------------------------------------------------
// 核心调度树遍历
// -------------------------------------------------------------

func (c *Html2MdConverter) walk(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	if n == nil {
		return
	}

	if ctx.depth > maxRecursionDepth {
		c.warnings.addMsg(fmt.Sprintf("Depth limit (%d)", maxRecursionDepth))
		return
	}

	// 1. 噪声过滤：隐藏元素、纯装饰性元素、复制代码按钮、KaTeX 纯视觉 DOM
	if isIgnoredNode(n, ctx) {
		return
	}

	// 2. 标题内置跳转锚点净化
	if ctx.inHeading && isHeadingAnchor(n) {
		return
	}

	ctx.depth++
	defer func() { ctx.depth-- }()

	switch n.Type {
	case nethtml.TextNode:
		c.handleText(n, buf, ctx)

	case nethtml.ElementNode:
		tag := strings.ToLower(n.Data)

		// ---------------------------------------------------------
		// 启发式搜索阶段 1：复杂语义与现代交互组件嗅探
		// ---------------------------------------------------------

		// 1.1 数学公式识别 (KaTeX / MathJax / MathML)
		if c.handleMath(n, buf) {
			return
		}

		// 1.2 无障碍伪表格支持：识别 role="table" 或 role="grid"
		if role := strings.ToLower(getAttr(n, "role")); role == "table" || role == "grid" {
			c.handleTable(n, buf, ctx)
			c.warnings.addMsg(fmt.Sprintf("ARIA table <%s>", tag))
			return
		}

		// 1.3 面包屑导航检测 (Breadcrumb)
		if isBreadcrumbNav(n) {
			c.handleBreadcrumb(n, buf, ctx)
			return
		}

		// 1.4 现代 Callout / Alert / Admonition 提示框检测
		if isCallout, calloutType := detectCallout(n); isCallout {
			c.handleCallout(n, calloutType, buf, ctx)
			return
		}

		// 1.5 现代无障碍模拟列表 (role="list")
		if role := strings.ToLower(getAttr(n, "role")); role == "list" && tag != "ul" && tag != "ol" {
			c.handleAriaList(n, buf, ctx)
			return
		}

		// 1.6 现代 Flex 双列键值对检测
		if kNode, vNode, ok := isKeyValueFlexRow(n); ok {
			c.handleKeyValueRow(kNode, vNode, buf, ctx)
			return
		}

		// 1.7 现代多列布局探测（CSS Grid / Multi-column / Bootstrap / Tailwind 响应式）
		if colCount := detectGridColumns(n); colCount >= 2 {
			if isTabularGrid(n, colCount) {
				c.handleTabularGrid(n, colCount, buf, ctx)
				return
			}
			if isMultiColumnCards(n, colCount) {
				c.handleMultiColumnCards(n, buf, ctx)
				return
			}
		}

		// ---------------------------------------------------------
		// 启发式搜索阶段 2：标准与语义 HTML 标签调度
		// ---------------------------------------------------------
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(tag[1] - '0')
			if level < 1 || level > 6 {
				level = 1
			}
			buf.EnsureNewlines(2)
			buf.WriteString(strings.Repeat("#", level) + " ")
			oldInHeading := ctx.inHeading
			ctx.inHeading = true
			c.walkChildren(n, buf, ctx)
			ctx.inHeading = oldInHeading
			buf.EnsureNewlines(2)

		case "p":
			if ctx.atListItemHead {
				ctx.atListItemHead = false
			} else {
				buf.EnsureNewlines(2)
			}
			c.walkChildren(n, buf, ctx)
			buf.EnsureNewlines(2)

		case "hr":
			buf.EnsureNewlines(2)
			buf.WriteString("---")
			buf.EnsureNewlines(2)

		case "br":
			buf.WriteString("  \n")

		case "strong", "b":
			c.handleInlineWrap(n, buf, "**", ctx)

		case "em", "i":
			c.handleInlineWrap(n, buf, "*", ctx)

		case "del", "s", "strike":
			c.handleInlineWrap(n, buf, "~~", ctx)

		case "mark":
			c.handleInlineWrap(n, buf, "==", ctx)

		case "kbd":
			temp := newMdBuffer()
			c.walkChildren(n, temp, ctx)
			txt := strings.TrimSpace(temp.String())
			if txt != "" {
				buf.WriteString("<kbd>" + html.EscapeString(txt) + "</kbd>")
			}

		case "time":
			c.walkChildren(n, buf, ctx)

		case "u", "sub", "sup":
			buf.WriteString("<" + tag + ">")
			c.walkChildren(n, buf, ctx)
			buf.WriteString("</" + tag + ">")

		case "abbr":
			title := getAttr(n, "title")
			temp := newMdBuffer()
			c.walkChildren(n, temp, ctx)
			content := strings.TrimSpace(temp.String())
			if title != "" && content != "" && !strings.EqualFold(title, content) {
				buf.WriteString(fmt.Sprintf("%s (%s)", content, title))
			} else {
				buf.WriteString(content)
			}

		case "q":
			buf.WriteString("“")
			c.walkChildren(n, buf, ctx)
			buf.WriteString("”")

		case "code":
			if !ctx.inPre {
				c.handleInlineCode(n, buf)
			} else {
				c.walkChildren(n, buf, ctx)
			}

		case "pre":
			c.handlePreCodeBlock(n, buf, ctx)

		case "blockquote":
			c.handleBlockquote(n, buf, ctx)

		case "aside":
			c.handleAside(n, buf, ctx)

		case "ul", "ol":
			c.handleList(n, buf, ctx, tag == "ol")

		case "dl":
			buf.EnsureNewlines(2)
			c.walkChildren(n, buf, ctx)
			buf.EnsureNewlines(2)

		case "dt":
			buf.EnsureNewlines(1)
			buf.WriteString("**")
			c.walkChildren(n, buf, ctx)
			buf.WriteString("**  \n")

		case "dd":
			buf.WriteString(": ")
			c.walkChildren(n, buf, ctx)
			buf.EnsureNewlines(1)

		case "table":
			c.handleTable(n, buf, ctx)

		case "a":
			c.handleAnchor(n, buf, ctx)

		case "img":
			c.handleImage(n, buf)

		case "figure":
			buf.EnsureNewlines(2)
			c.walkChildren(n, buf, ctx)
			buf.EnsureNewlines(2)

		case "figcaption":
			buf.EnsureNewlines(1)
			temp := newMdBuffer()
			c.walkChildren(n, temp, ctx)
			caption := strings.TrimSpace(temp.String())
			if caption != "" {
				buf.WriteString("*" + caption + "*")
			}
			buf.EnsureNewlines(2)

		case "details":
			if c.opts.PreserveDetails {
				buf.EnsureNewlines(2)
				buf.WriteString("<details>\n")
				c.walkChildren(n, buf, ctx)
				buf.EnsureNewlines(1)
				buf.WriteString("</details>")
				buf.EnsureNewlines(2)
			} else {
				c.walkChildren(n, buf, ctx)
			}

		case "summary":
			if c.opts.PreserveDetails {
				buf.WriteString("<summary>")
				c.walkChildren(n, buf, ctx)
				buf.WriteString("</summary>\n\n")
			} else {
				buf.EnsureNewlines(1)
				buf.WriteString("**")
				c.walkChildren(n, buf, ctx)
				buf.WriteString("**\n\n")
			}

		case "input":
			inputType := strings.ToLower(getAttr(n, "type"))
			if inputType == "text" || inputType == "search" || inputType == "" {
				if val := getAttr(n, "value"); val != "" {
					buf.WriteString("`" + val + "`")
				}
			}

		case "textarea":
			temp := extractRawText(n)
			if strings.TrimSpace(temp) != "" {
				fence := "```"
				for strings.Contains(temp, fence) {
					fence += "`"
				}
				buf.EnsureNewlines(1)
				buf.WriteString(fence + "\n" + temp + "\n" + fence)
				buf.EnsureNewlines(1)
			}

		case "video", "audio", "iframe", "canvas", "object":
			c.handleMediaFallback(n, buf, ctx)

		case "div", "section", "article", "header", "footer", "main", "nav", "html", "body", "center":
			// A block element at the head of a list item shares the bullet's
			// line; otherwise the bullet would dangle on a line of its own.
			if ctx.atListItemHead {
				ctx.atListItemHead = false
			} else {
				buf.EnsureNewlines(1)
			}
			c.walkChildren(n, buf, ctx)
			buf.EnsureNewlines(1)

		// <span>/<font>/<small>/<big> are inline elements. Emitting block
		// newlines around them splits inline content away from its surrounding
		// text (e.g. the bullet of a list item, or a heading's link text).
		case "span", "font", "small", "big":
			c.walkChildren(n, buf, ctx)

		default:
			if n.FirstChild != nil {
				c.warnings.addTag(tag)
				c.walkChildren(n, buf, ctx)
			} else {
				label := getAttr(n, "aria-label")
				if label == "" {
					label = getAttr(n, "alt")
				}
				if label == "" {
					label = getAttr(n, "title")
				}
				if label != "" {
					buf.WriteString(" " + label + " ")
					c.warnings.addMsg(fmt.Sprintf("Unknown <%s>", tag))
				} else {
					c.warnings.addMsg(fmt.Sprintf("Skipped <%s>", tag))
				}
			}
		}

	default:
		// Document / doctype nodes: descend into children.
		c.walkChildren(n, buf, ctx)
	}
}

func (c *Html2MdConverter) walkChildren(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		c.walk(child, buf, ctx)
	}
}

// -------------------------------------------------------------
// 现代启发式处理器 1：数学公式处理（KaTeX / MathJax / MathML）
// -------------------------------------------------------------

func (c *Html2MdConverter) handleMath(n *nethtml.Node, buf *mdBuffer) bool {
	tag := strings.ToLower(n.Data)
	class := getAttr(n, "class")

	isKaTeX := strings.Contains(class, "katex")
	isMathJax := strings.Contains(class, "MathJax") || tag == "mjx-container"
	isMathML := tag == "math"
	hasTexAttr := hasAttr(n, "data-tex") || hasAttr(n, "data-latex")

	if !isKaTeX && !isMathJax && !isMathML && !hasTexAttr {
		return false
	}

	isBlock := strings.Contains(class, "katex-display") ||
		strings.Contains(class, "MathJax_Display") ||
		getAttr(n, "display") == "block" ||
		getAttr(n, "display") == "true"

	tex := ""
	if v := getAttr(n, "data-tex"); v != "" {
		tex = v
	} else if v := getAttr(n, "data-latex"); v != "" {
		tex = v
	}

	if tex == "" {
		var findTex func(*nethtml.Node)
		findTex = func(curr *nethtml.Node) {
			if curr == nil || tex != "" {
				return
			}
			if curr.Type == nethtml.ElementNode {
				cTag := strings.ToLower(curr.Data)
				if cTag == "annotation" && strings.Contains(strings.ToLower(getAttr(curr, "encoding")), "tex") {
					tex = extractRawText(curr)
					return
				}
				if cTag == "script" && strings.Contains(strings.ToLower(getAttr(curr, "type")), "math/tex") {
					tex = extractRawText(curr)
					return
				}
			}
			for ch := curr.FirstChild; ch != nil; ch = ch.NextSibling {
				findTex(ch)
			}
		}
		findTex(n)
	}

	tex = strings.TrimSpace(tex)
	if tex == "" {
		return false
	}

	if isBlock {
		buf.EnsureNewlines(2)
		buf.WriteString("$$\n" + tex + "\n$$")
		buf.EnsureNewlines(2)
	} else {
		buf.WriteString("$" + tex + "$")
	}

	c.warnings.addMsg("Math formula extracted")
	return true
}

// -------------------------------------------------------------
// 现代启发式处理器 2：Callout / Admonition / Alert 提示框
// -------------------------------------------------------------

func detectCallout(n *nethtml.Node) (bool, string) {
	tag := strings.ToLower(n.Data)
	switch tag {
	case "div", "aside", "section", "blockquote":
	default:
		return false, ""
	}

	class := strings.ToLower(getAttr(n, "class"))
	role := strings.ToLower(getAttr(n, "role"))

	if !strings.Contains(class, "alert") &&
		!strings.Contains(class, "callout") &&
		!strings.Contains(class, "admonition") &&
		!strings.Contains(class, "notice") &&
		role != "alert" {
		return false, ""
	}

	if isLeafOrShortText(n) {
		return false, ""
	}

	allAttrs := class + " " + strings.ToLower(getAttr(n, "data-type"))
	var calloutType string
	switch {
	case strings.Contains(allAttrs, "tip") || strings.Contains(allAttrs, "success") || strings.Contains(allAttrs, "solved"):
		calloutType = "TIP"
	case strings.Contains(allAttrs, "warning") || strings.Contains(allAttrs, "warn"):
		calloutType = "WARNING"
	case strings.Contains(allAttrs, "danger") || strings.Contains(allAttrs, "error") || strings.Contains(allAttrs, "failure") || strings.Contains(allAttrs, "bug"):
		calloutType = "CAUTION"
	case strings.Contains(allAttrs, "important") || strings.Contains(allAttrs, "critical"):
		calloutType = "IMPORTANT"
	default:
		calloutType = "NOTE"
	}

	return true, calloutType
}

func (c *Html2MdConverter) handleCallout(n *nethtml.Node, calloutType string, buf *mdBuffer, ctx *walkContext) {
	temp := newMdBuffer()
	c.walkChildren(n, temp, ctx)
	inner := strings.Trim(temp.String(), "\n")

	if inner == "" {
		return
	}

	lines := strings.Split(inner, "\n")
	filteredLines := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.Trim(strings.TrimSpace(line), "*_# :")
		if i == 0 && strings.EqualFold(trimmed, calloutType) {
			continue
		}
		filteredLines = append(filteredLines, line)
	}

	buf.EnsureNewlines(2)
	buf.WriteString("> [!" + calloutType + "]\n")
	for _, line := range filteredLines {
		if strings.TrimSpace(line) == "" {
			buf.WriteString(">\n")
		} else {
			buf.WriteString("> " + line + "\n")
		}
	}
	buf.EnsureNewlines(2)
	c.warnings.addMsg(fmt.Sprintf("Callout [%s]", calloutType))
}

// -------------------------------------------------------------
// 现代启发式处理器 3：面包屑导航 (Breadcrumbs)
// -------------------------------------------------------------

func isBreadcrumbNav(n *nethtml.Node) bool {
	tag := strings.ToLower(n.Data)
	class := strings.ToLower(getAttr(n, "class"))
	ariaLabel := strings.ToLower(getAttr(n, "aria-label"))
	return (tag == "nav" || tag == "div") &&
		(strings.Contains(ariaLabel, "breadcrumb") || strings.Contains(class, "breadcrumb"))
}

func (c *Html2MdConverter) handleBreadcrumb(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	var items []string
	var collectItems func(*nethtml.Node)
	collectItems = func(curr *nethtml.Node) {
		if curr == nil {
			return
		}
		if curr.Type == nethtml.ElementNode {
			cTag := strings.ToLower(curr.Data)
			if cTag == "li" || cTag == "a" || hasAttr(curr, "data-breadcrumb-item") {
				temp := newMdBuffer()
				c.walkChildren(curr, temp, ctx)
				txt := strings.TrimSpace(temp.String())
				if txt != "" && txt != "/" && txt != ">" && txt != "›" {
					items = append(items, txt)
				}
				if cTag == "li" {
					return
				}
			}
		}
		for ch := curr.FirstChild; ch != nil; ch = ch.NextSibling {
			collectItems(ch)
		}
	}
	collectItems(n)

	if len(items) == 0 {
		c.walkChildren(n, buf, ctx)
		return
	}

	buf.EnsureNewlines(2)
	buf.WriteString(strings.Join(items, " > "))
	buf.EnsureNewlines(2)
	c.warnings.addMsg("Breadcrumb linearized")
}

// -------------------------------------------------------------
// 现代启发式处理器 4：ARIA 模拟列表 (role="list")
// -------------------------------------------------------------

func (c *Html2MdConverter) handleAriaList(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	buf.EnsureNewlines(1)
	savedDepth := ctx.listDepth
	savedOrdered := ctx.isOrdered
	savedCounter := ctx.listCounter

	ctx.listDepth++
	ctx.isOrdered = false
	ctx.listCounter = 1

	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == nethtml.ElementNode {
			role := strings.ToLower(getAttr(child, "role"))
			class := strings.ToLower(getAttr(child, "class"))
			if role == "listitem" || strings.Contains(class, "list-item") {
				c.handleListItem(child, buf, ctx)
			}
		}
	}

	ctx.listDepth = savedDepth
	ctx.isOrdered = savedOrdered
	ctx.listCounter = savedCounter
	buf.EnsureNewlines(1)
	c.warnings.addMsg("ARIA list")
}

// -------------------------------------------------------------
// 现代启发式处理器 5：多列与响应式网格嗅探
// -------------------------------------------------------------

func getCleanElementChildren(n *nethtml.Node) []*nethtml.Node {
	var elements []*nethtml.Node
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == nethtml.ElementNode && !isIgnoredNode(ch, nil) {
			elements = append(elements, ch)
		}
	}
	return elements
}

func detectGridColumns(n *nethtml.Node) int {
	class := getAttr(n, "class")
	style := getAttr(n, "style")

	if matches := tailwindGridRegex.FindAllStringSubmatch(class, -1); len(matches) > 0 {
		maxCol := 0
		for _, m := range matches {
			if len(m) > 1 {
				if cnt, err := strconv.Atoi(m[1]); err == nil && cnt > maxCol {
					maxCol = cnt
				}
			}
		}
		if maxCol > 0 {
			return maxCol
		}
	}

	if matches := tailwindColsRegex.FindAllStringSubmatch(class, -1); len(matches) > 0 {
		maxCol := 0
		for _, m := range matches {
			if len(m) > 1 {
				if cnt, err := strconv.Atoi(m[1]); err == nil && cnt > maxCol {
					maxCol = cnt
				}
			}
		}
		if maxCol > 0 {
			return maxCol
		}
	}

	if m := cssGridRepRegex.FindStringSubmatch(style); len(m) > 1 {
		if cnt, err := strconv.Atoi(m[1]); err == nil && cnt > 0 {
			return cnt
		}
	}

	if strings.Contains(style, "grid-template-columns:") {
		if cols := parseInlineGridCols(style); cols >= 2 {
			return cols
		}
	}

	if m := cssColCountRegex.FindStringSubmatch(style); len(m) > 1 {
		if cnt, err := strconv.Atoi(m[1]); err == nil && cnt > 0 {
			return cnt
		}
	}

	if strings.Contains(class, "row") {
		colCount := 0
		for _, ch := range getCleanElementChildren(n) {
			if strings.Contains(getAttr(ch, "class"), "col") {
				colCount++
			}
		}
		if colCount >= 2 {
			return colCount
		}
	}

	if strings.Contains(class, "flex") && !strings.Contains(class, "flex-col") {
		colCount := 0
		for _, ch := range getCleanElementChildren(n) {
			cClass := getAttr(ch, "class")
			if strings.Contains(cClass, "w-1/2") || strings.Contains(cClass, "w-1/3") ||
				strings.Contains(cClass, "w-1/4") || strings.Contains(cClass, "flex-1") {
				colCount++
			}
		}
		if colCount >= 2 {
			return colCount
		}
	}

	return 0
}

func parseInlineGridCols(style string) int {
	idx := strings.Index(style, "grid-template-columns:")
	if idx == -1 {
		return 0
	}
	val := style[idx+len("grid-template-columns:"):]
	if end := strings.Index(val, ";"); end != -1 {
		val = val[:end]
	}
	val = strings.TrimSpace(val)

	tokens := strings.Fields(val)
	count := 0
	for _, tok := range tokens {
		if strings.HasSuffix(tok, "fr") || strings.HasSuffix(tok, "px") ||
			strings.HasSuffix(tok, "%") || tok == "auto" {
			count++
		}
	}
	return count
}

func isTabularGrid(n *nethtml.Node, cols int) bool {
	if cols < 2 || cols > 8 {
		return false
	}
	elements := getCleanElementChildren(n)
	if len(elements) < cols*2 || len(elements)%cols != 0 {
		return false
	}
	for _, el := range elements {
		if hasBlockElements(el) {
			return false
		}
	}
	return true
}

func (c *Html2MdConverter) handleTabularGrid(n *nethtml.Node, cols int, buf *mdBuffer, ctx *walkContext) {
	elements := getCleanElementChildren(n)
	var rows []tableRow
	numRows := len(elements) / cols

	for r := 0; r < numRows; r++ {
		var row tableRow
		if r == 0 {
			row.isHeader = true
		}
		for colIdx := 0; colIdx < cols; colIdx++ {
			cellNode := elements[r*cols+colIdx]
			temp := newMdBuffer()
			c.walkChildren(cellNode, temp, ctx)
			text := strings.TrimSpace(temp.String())
			text = strings.ReplaceAll(text, "\r\n", "<br>")
			text = strings.ReplaceAll(text, "\n", "<br>")
			text = strings.ReplaceAll(text, "|", "\\|")
			row.cells = append(row.cells, tableCell{text: text, align: "left"})
		}
		rows = append(rows, row)
	}

	c.renderTableRows(rows, buf)
	c.warnings.addMsg(fmt.Sprintf("Grid (%d cols) -> table", cols))
}

func isKeyValueFlexRow(n *nethtml.Node) (*nethtml.Node, *nethtml.Node, bool) {
	class := getAttr(n, "class")
	style := getAttr(n, "style")
	isFlex := strings.Contains(class, "flex") || strings.Contains(style, "display: flex") || strings.Contains(style, "display:flex")
	if !isFlex {
		return nil, nil, false
	}
	if strings.Contains(class, "flex-col") || strings.Contains(style, "flex-direction: column") {
		return nil, nil, false
	}

	children := getCleanElementChildren(n)
	if len(children) != 2 {
		return nil, nil, false
	}
	if hasBlockElements(children[0]) || hasBlockElements(children[1]) {
		return nil, nil, false
	}

	txt1 := extractRawText(children[0])
	txt2 := extractRawText(children[1])
	if utf8.RuneCountInString(txt1)+utf8.RuneCountInString(txt2) > 120 {
		return nil, nil, false
	}
	if txt1 == "" && txt2 == "" {
		return nil, nil, false
	}
	return children[0], children[1], true
}

func (c *Html2MdConverter) handleKeyValueRow(keyNode, valNode *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	tempK := newMdBuffer()
	c.walk(keyNode, tempK, ctx)
	k := strings.TrimSpace(tempK.String())

	tempV := newMdBuffer()
	c.walk(valNode, tempV, ctx)
	v := strings.TrimSpace(tempV.String())

	if k == "" && v == "" {
		return
	}

	buf.EnsureNewlines(1)
	if k != "" && v != "" {
		if strings.HasPrefix(k, "**") && strings.HasSuffix(k, "**") {
			buf.WriteString(fmt.Sprintf("- %s: %s", k, v))
		} else {
			buf.WriteString(fmt.Sprintf("- **%s**: %s", k, v))
		}
	} else if k != "" {
		buf.WriteString(fmt.Sprintf("- %s", k))
	} else {
		buf.WriteString(fmt.Sprintf("- %s", v))
	}
	buf.EnsureNewlines(1)
	c.warnings.addMsg("Flex KV")
}

func isMultiColumnCards(n *nethtml.Node, cols int) bool {
	return len(getCleanElementChildren(n)) >= 2
}

func (c *Html2MdConverter) handleMultiColumnCards(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	elements := getCleanElementChildren(n)
	buf.EnsureNewlines(2)
	for i, colNode := range elements {
		if i > 0 {
			buf.EnsureNewlines(2)
		}
		c.walk(colNode, buf, ctx)
	}
	buf.EnsureNewlines(2)
	c.warnings.addMsg(fmt.Sprintf("Multi-col (%d)", len(elements)))
}

func (c *Html2MdConverter) handleAside(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	temp := newMdBuffer()
	c.walkChildren(n, temp, ctx)
	inner := strings.Trim(temp.String(), "\n")
	if inner == "" {
		return
	}

	buf.EnsureNewlines(2)
	lines := strings.Split(inner, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			buf.WriteString(">\n")
		} else {
			buf.WriteString("> " + line + "\n")
		}
	}
	buf.EnsureNewlines(2)
	c.warnings.addMsg("Aside")
}

func hasBlockElements(n *nethtml.Node) bool {
	var found bool
	var walk func(*nethtml.Node)
	walk = func(curr *nethtml.Node) {
		if curr == nil || found {
			return
		}
		if curr.Type == nethtml.ElementNode {
			switch strings.ToLower(curr.Data) {
			case "p", "table", "pre", "blockquote", "h1", "h2", "h3", "ul", "ol":
				found = true
				return
			}
		}
		for c := curr.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return found
}

// -------------------------------------------------------------
// 代码块提取与语言嗅探
// -------------------------------------------------------------

func (c *Html2MdConverter) handlePreCodeBlock(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	var codeNode *nethtml.Node
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == nethtml.ElementNode && strings.ToLower(child.Data) == "code" {
			codeNode = child
			break
		}
	}

	lang := extractLanguage(n, codeNode)
	target := n
	if codeNode != nil {
		target = codeNode
	}

	oldInPre := ctx.inPre
	ctx.inPre = true
	code := extractCodeBlockText(target)
	ctx.inPre = oldInPre

	fence := c.opts.CodeFence
	if fence == "" {
		fence = "```"
	}
	for strings.Contains(code, fence) {
		fence += "`"
	}

	buf.EnsureNewlines(2)
	buf.WriteString(fence + lang + "\n")
	buf.WriteString(code)
	if !strings.HasSuffix(code, "\n") {
		buf.WriteString("\n")
	}
	buf.WriteString(fence)
	buf.EnsureNewlines(2)
}

func extractCodeBlockText(n *nethtml.Node) string {
	var sb strings.Builder
	var walk func(*nethtml.Node)
	walk = func(curr *nethtml.Node) {
		if curr == nil {
			return
		}
		if curr.Type == nethtml.ElementNode {
			tag := strings.ToLower(curr.Data)
			class := strings.ToLower(getAttr(curr, "class"))

			if strings.Contains(class, "line-number") ||
				strings.Contains(class, "linenumber") ||
				strings.Contains(class, "blob-num") {
				return
			}

			if tag == "br" {
				sb.WriteByte('\n')
				return
			}

			for c := curr.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}

			if tag == "div" || tag == "p" || tag == "tr" {
				if sb.Len() > 0 && sb.String()[sb.Len()-1] != '\n' {
					sb.WriteByte('\n')
				}
			}
			return
		}

		if curr.Type == nethtml.TextNode {
			sb.WriteString(curr.Data)
		}
	}
	walk(n)
	return sb.String()
}

func extractLanguage(preNode, codeNode *nethtml.Node) string {
	candidates := []*nethtml.Node{codeNode, preNode}
	if preNode != nil && preNode.Parent != nil {
		candidates = append(candidates, preNode.Parent)
		if preNode.Parent.Parent != nil {
			candidates = append(candidates, preNode.Parent.Parent)
		}
	}

	for _, n := range candidates {
		if n == nil {
			continue
		}
		if v := getAttr(n, "data-lang"); v != "" {
			return v
		}
		if v := getAttr(n, "data-language"); v != "" {
			return v
		}
		if v := getAttr(n, "data-code-language"); v != "" {
			return v
		}
		class := getAttr(n, "class")
		if m := langRegex.FindStringSubmatch(class); len(m) > 1 {
			return m[1]
		}
		if m := ghSourceLangRegex.FindStringSubmatch(class); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

// -------------------------------------------------------------
// 文本与标准行内格式化（含 XSS 防御）
// -------------------------------------------------------------

func (c *Html2MdConverter) handleText(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	if ctx.inPre {
		buf.WriteString(n.Data)
		return
	}

	text := n.Data
	if text == "" {
		return
	}

	text = strings.ReplaceAll(text, "<", "&lt;")

	var sb strings.Builder
	wasSpace := false
	for _, r := range text {
		if r == '\u200B' || r == '\uFEFF' {
			continue
		}
		if unicode.IsSpace(r) || r == '\u00A0' {
			if !wasSpace {
				sb.WriteByte(' ')
				wasSpace = true
			}
		} else {
			sb.WriteRune(r)
			wasSpace = false
		}
	}

	collapsed := sb.String()
	if collapsed == "" {
		return
	}

	if collapsed == " " {
		last := buf.LastByte()
		if last == 0 || last == ' ' || last == '\n' {
			return
		}
		_ = buf.WriteByte(' ')
		return
	}

	if collapsed[0] == ' ' {
		last := buf.LastByte()
		if last == 0 || last == ' ' || last == '\n' {
			collapsed = collapsed[1:]
		}
	}

	buf.WriteString(collapsed)
}

func (c *Html2MdConverter) handleInlineWrap(n *nethtml.Node, buf *mdBuffer, delimiter string, ctx *walkContext) {
	temp := newMdBuffer()
	c.walkChildren(n, temp, ctx)
	content := temp.String()
	if strings.TrimSpace(content) == "" {
		buf.WriteString(content)
		return
	}

	runes := []rune(content)
	start := 0
	for start < len(runes) && unicode.IsSpace(runes[start]) {
		start++
	}
	leadingSpace := string(runes[:start])

	end := len(runes)
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}
	trailingSpace := string(runes[end:])
	trimmed := string(runes[start:end])

	if leadingSpace != "" && (buf.LastByte() == ' ' || buf.LastByte() == '\n') {
		leadingSpace = ""
	}

	buf.WriteString(leadingSpace + delimiter + trimmed + delimiter + trailingSpace)
}

func (c *Html2MdConverter) handleInlineCode(n *nethtml.Node, buf *mdBuffer) {
	codeText := extractRawText(n)
	if codeText == "" {
		return
	}
	codeText = strings.ReplaceAll(codeText, "\r\n", " ")
	codeText = strings.ReplaceAll(codeText, "\n", " ")

	fence := "`"
	for strings.Contains(codeText, fence) {
		fence += "`"
	}

	if strings.HasPrefix(codeText, "`") || strings.HasSuffix(codeText, "`") {
		buf.WriteString(fence + " " + codeText + " " + fence)
	} else {
		buf.WriteString(fence + codeText + fence)
	}
}

func (c *Html2MdConverter) handleBlockquote(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	temp := newMdBuffer()
	c.walkChildren(n, temp, ctx)
	inner := strings.Trim(temp.String(), "\n")
	if inner == "" {
		return
	}

	buf.EnsureNewlines(2)
	lines := strings.Split(inner, "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			buf.WriteString(">\n")
		} else {
			buf.WriteString("> " + line + "\n")
		}
	}
	buf.EnsureNewlines(2)
}

func (c *Html2MdConverter) handleList(n *nethtml.Node, buf *mdBuffer, ctx *walkContext, isOrdered bool) {
	buf.EnsureNewlines(1)
	savedDepth := ctx.listDepth
	savedOrdered := ctx.isOrdered
	savedCounter := ctx.listCounter

	ctx.listDepth++
	ctx.isOrdered = isOrdered
	ctx.listCounter = 1

	if isOrdered {
		if startVal, err := strconv.Atoi(getAttr(n, "start")); err == nil && startVal > 0 {
			ctx.listCounter = startVal
		}
	}

	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == nethtml.ElementNode && strings.ToLower(child.Data) == "li" {
			c.handleListItem(child, buf, ctx)
		}
	}

	ctx.listDepth = savedDepth
	ctx.isOrdered = savedOrdered
	ctx.listCounter = savedCounter
	buf.EnsureNewlines(1)
}

func (c *Html2MdConverter) handleListItem(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	indentCount := ctx.listDepth - 1
	if indentCount < 0 {
		indentCount = 0
	}
	indent := strings.Repeat("  ", indentCount)
	prefix := indent + c.opts.BulletMarker + " "
	if ctx.isOrdered {
		prefix = indent + fmt.Sprintf("%d. ", ctx.listCounter)
		ctx.listCounter++
	}

	checkboxNode, hasCheck, isChecked := findCheckboxOrTask(n)
	if hasCheck {
		if isChecked {
			prefix += "[x] "
		} else {
			prefix += "[ ] "
		}
	}

	buf.EnsureNewlines(1)
	buf.WriteString(prefix)

	oldAtHead := ctx.atListItemHead
	ctx.atListItemHead = true

	// Render the item body into a scratch buffer so that leading blank lines
	// emitted by nested block elements can be dropped; otherwise the bullet
	// would be left dangling on a line of its own (e.g. "1.\n![img](src)"
	// instead of "1. ![img](src)").
	item := newMdBuffer()
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child == checkboxNode {
			continue
		}
		c.walk(child, item, ctx)
	}
	buf.WriteString(strings.TrimLeft(item.String(), "\n"))

	ctx.atListItemHead = oldAtHead
	buf.EnsureNewlines(1)
}

// -------------------------------------------------------------
// 表格渲染系统
// -------------------------------------------------------------

type tableCell struct {
	text  string
	align string
}

type tableRow struct {
	cells    []tableCell
	isHeader bool
}

func (c *Html2MdConverter) handleTable(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	var rows []tableRow
	c.collectTableRows(n, &rows, false, ctx)
	if len(rows) == 0 {
		return
	}
	c.renderTableRows(rows, buf)
}

func (c *Html2MdConverter) renderTableRows(rows []tableRow, buf *mdBuffer) {
	maxCols := 0
	for _, r := range rows {
		if len(r.cells) > maxCols {
			maxCols = len(r.cells)
		}
	}
	if maxCols == 0 {
		return
	}

	for i := range rows {
		for len(rows[i].cells) < maxCols {
			rows[i].cells = append(rows[i].cells, tableCell{})
		}
	}

	colAligns := make([]string, maxCols)
	for colIdx := 0; colIdx < maxCols; colIdx++ {
		for _, r := range rows {
			if r.cells[colIdx].align != "" {
				colAligns[colIdx] = r.cells[colIdx].align
				break
			}
		}
	}

	colWidths := make([]int, maxCols)
	for colIdx := 0; colIdx < maxCols; colIdx++ {
		minW := 3
		if colAligns[colIdx] == "center" {
			minW = 5
		} else if colAligns[colIdx] == "right" || colAligns[colIdx] == "left" {
			minW = 4
		}
		colWidths[colIdx] = minW

		for _, r := range rows {
			w := utf8.RuneCountInString(r.cells[colIdx].text)
			if w > colWidths[colIdx] {
				colWidths[colIdx] = w
			}
		}
	}

	buf.EnsureNewlines(2)

	headerRow := rows[0]
	dataRows := rows[1:]

	printRow := func(r tableRow) {
		buf.WriteString("|")
		for i, cell := range r.cells {
			pad := colWidths[i] - utf8.RuneCountInString(cell.text)
			if pad < 0 {
				pad = 0
			}
			buf.WriteString(" " + cell.text + strings.Repeat(" ", pad) + " |")
		}
		buf.WriteString("\n")
	}

	printRow(headerRow)

	buf.WriteString("|")
	for i := 0; i < maxCols; i++ {
		w := colWidths[i]
		switch colAligns[i] {
		case "center":
			dashCount := w - 2
			if dashCount < 1 {
				dashCount = 1
			}
			buf.WriteString(" :" + strings.Repeat("-", dashCount) + ": |")
		case "right":
			dashCount := w - 1
			if dashCount < 1 {
				dashCount = 1
			}
			buf.WriteString(" " + strings.Repeat("-", dashCount) + ": |")
		case "left":
			dashCount := w - 1
			if dashCount < 1 {
				dashCount = 1
			}
			buf.WriteString(" :" + strings.Repeat("-", dashCount) + " |")
		default:
			if w < 1 {
				w = 1
			}
			buf.WriteString(" " + strings.Repeat("-", w) + " |")
		}
	}
	buf.WriteString("\n")

	for _, r := range dataRows {
		printRow(r)
	}
	buf.EnsureNewlines(2)
}

func (c *Html2MdConverter) collectTableRows(n *nethtml.Node, rows *[]tableRow, inHeader bool, ctx *walkContext) {
	if n.Type == nethtml.ElementNode {
		tag := strings.ToLower(n.Data)
		role := strings.ToLower(getAttr(n, "role"))

		if tag == "thead" || role == "rowgroup" && hasAttr(n, "data-header") {
			inHeader = true
		}

		if tag == "tr" || role == "row" {
			var r tableRow
			r.isHeader = inHeader
			for cellNode := n.FirstChild; cellNode != nil; cellNode = cellNode.NextSibling {
				if cellNode.Type == nethtml.ElementNode {
					cTag := strings.ToLower(cellNode.Data)
					cRole := strings.ToLower(getAttr(cellNode, "role"))

					isHeaderCell := cTag == "th" || cRole == "columnheader" || cRole == "rowheader"
					isDataCell := cTag == "td" || cRole == "cell" || cRole == "gridcell"

					if isHeaderCell || isDataCell {
						if isHeaderCell {
							r.isHeader = true
						}
						temp := newMdBuffer()
						c.walkChildren(cellNode, temp, ctx)
						text := temp.String()
						text = strings.ReplaceAll(text, "\r\n", "<br>")
						text = strings.ReplaceAll(text, "\n", "<br>")
						text = strings.ReplaceAll(text, "|", "\\|")
						text = strings.TrimSpace(text)

						align := getTableAlign(cellNode)
						r.cells = append(r.cells, tableCell{text: text, align: align})

						if cs, err := strconv.Atoi(getAttr(cellNode, "colspan")); err == nil && cs > 1 {
							for k := 1; k < cs; k++ {
								r.cells = append(r.cells, tableCell{text: "", align: align})
							}
						}
					}
				}
			}
			if len(r.cells) > 0 {
				*rows = append(*rows, r)
			}
			return
		}
	}

	for child := n.FirstChild; child != nil; child = child.NextSibling {
		c.collectTableRows(child, rows, inHeader, ctx)
	}
}

// -------------------------------------------------------------
// 媒体与链接处理器（含安全清洗与协议校验）
// -------------------------------------------------------------

func (c *Html2MdConverter) handleMediaFallback(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	tag := strings.ToLower(n.Data)
	temp := newMdBuffer()
	c.walkChildren(n, temp, ctx)
	innerText := strings.TrimSpace(temp.String())

	if innerText != "" {
		buf.EnsureNewlines(1)
		buf.WriteString(innerText)
		buf.EnsureNewlines(1)
		c.warnings.addMsg(fmt.Sprintf("Media <%s>", tag))
		return
	}

	label := getAttr(n, "aria-label")
	if label == "" {
		label = getAttr(n, "title")
	}
	if label == "" {
		label = getAttr(n, "alt")
	}
	label = escapeMarkdownBrackets(label)

	safeSrc := sanitizeURL(c.resolveURL(getAttr(n, "src")), false)

	if safeSrc != "" && label != "" {
		buf.WriteString(fmt.Sprintf("[%s: %s](%s)", strings.ToUpper(tag), label, safeSrc))
		c.warnings.addMsg(fmt.Sprintf("Media <%s>", tag))
	} else if safeSrc != "" {
		buf.WriteString(fmt.Sprintf("[%s: %s](%s)", strings.ToUpper(tag), safeSrc, safeSrc))
		c.warnings.addMsg(fmt.Sprintf("Media <%s>", tag))
	} else if label != "" {
		buf.WriteString(fmt.Sprintf("[%s: %s]", strings.ToUpper(tag), label))
		c.warnings.addMsg(fmt.Sprintf("Media <%s>", tag))
	} else {
		c.warnings.addMsg(fmt.Sprintf("Skipped <%s>", tag))
	}
}

func (c *Html2MdConverter) handleAnchor(n *nethtml.Node, buf *mdBuffer, ctx *walkContext) {
	rawHref := getAttr(n, "href")
	title := getAttr(n, "title")

	safeHref := sanitizeURL(c.resolveURL(rawHref), false)

	// A Markdown link must stay on a single line: its text cannot contain
	// newlines or block-level constructs. When the anchor wraps block-level
	// elements (h1-h6, p, table, ul, ...), walkChildren emits their full
	// Markdown (including "## " markers and blank lines), which would yield an
	// invalid multi-line link. In that case, attach the URL to the first
	// suitable piece of content at the top of the block and render the rest of
	// the block normally, without the link.
	//
	// This is checked before the inline conversion below, so that a <p> nested
	// in the anchor cannot consume ctx.atListItemHead while the anchor is still
	// being inspected.
	if firstBlock := findFirstBlockChild(n); firstBlock != nil {
		if safeHref == "" {
			c.walkChildren(n, buf, ctx)
			if rawHref != "" {
				c.warnings.addMsg("Dangerous URL filtered")
			}
			return
		}
		c.handleBlockLevelAnchor(n, firstBlock, buf, ctx, safeHref, title)
		return
	}

	temp := newMdBuffer()
	c.walkChildren(n, temp, ctx)
	text := temp.String()

	if safeHref == "" {
		buf.WriteString(text)
		if rawHref != "" {
			c.warnings.addMsg("Dangerous URL filtered")
		}
		return
	}

	// Inline content: collapse any newlines (e.g. from <br>) so the link text
	// stays on a single line.
	text = collapseInlineNewlines(text)
	if text == "" {
		text = safeHref
	}
	text = escapeLinkText(text)

	if title != "" {
		cleanTitle := strings.ReplaceAll(title, `"`, `\"`)
		buf.WriteString(fmt.Sprintf("[%s](%s %q)", text, safeHref, cleanTitle))
	} else {
		buf.WriteString(fmt.Sprintf("[%s](%s)", text, safeHref))
	}
}

// handleBlockLevelAnchor renders an <a> element that wraps block-level
// content. The URL is attached to the first suitable piece of content at the
// top of the block (the heading text when the block starts with a heading,
// otherwise the block's plain text); the remaining block content is rendered
// normally without the link.
func (c *Html2MdConverter) handleBlockLevelAnchor(n *nethtml.Node, firstBlock *nethtml.Node, buf *mdBuffer, ctx *walkContext, safeHref, title string) {
	tag := strings.ToLower(firstBlock.Data)
	linkText := extractPlainText(firstBlock)

	if linkText == "" {
		// Nothing meaningful to attach the link to; render the whole anchor
		// normally (URL dropped, matching the empty inline-text fallback).
		c.walkChildren(n, buf, ctx)
		return
	}

	// Inside a list item head the bullet already supplies the block structure,
	// so the heading marker is dropped and the bullet stays on the same line as
	// the link (e.g. "- [Title](url)" rather than "-\n### [Title](url)").
	keepHeading := isHeadingTag(tag)
	if ctx.atListItemHead {
		ctx.atListItemHead = false
		keepHeading = false
	}

	if keepHeading {
		level := int(tag[1] - '0')
		if level < 1 || level > 6 {
			level = 1
		}
		buf.EnsureNewlines(2)
		buf.WriteString(strings.Repeat("#", level) + " ")
	}
	if title != "" {
		cleanTitle := strings.ReplaceAll(title, `"`, `\"`)
		buf.WriteString(fmt.Sprintf("[%s](%s %q)", escapeLinkText(linkText), safeHref, cleanTitle))
	} else {
		buf.WriteString(fmt.Sprintf("[%s](%s)", escapeLinkText(linkText), safeHref))
	}
	if keepHeading {
		buf.EnsureNewlines(2)
	}

	// Render the rest of the anchor's direct children (skipping the block
	// consumed as the link target) without the link.
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child == firstBlock {
			continue
		}
		c.walk(child, buf, ctx)
	}
}

// findFirstBlockChild returns the first direct block-level child element of
// n, or nil if n has none.
func findFirstBlockChild(n *nethtml.Node) *nethtml.Node {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != nethtml.ElementNode {
			continue
		}
		if isBlockElement(strings.ToLower(child.Data)) {
			return child
		}
	}
	return nil
}

// isBlockElement reports whether the given tag is treated as a block-level
// element for the purpose of deciding how an anchor's content is rendered.
func isBlockElement(tag string) bool {
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6", "p", "div", "ul", "ol",
		"table", "figure", "blockquote", "pre", "hr", "section", "article",
		"header", "footer", "nav", "main", "aside", "details", "dl", "form",
		"address", "fieldset", "tr", "thead", "tbody", "tfoot":
		return true
	}
	return false
}

// isHeadingTag reports whether tag is one of h1..h6.
func isHeadingTag(tag string) bool {
	return len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6'
}

// extractPlainText returns the concatenated plain text of a node with all
// whitespace collapsed to single spaces.
func extractPlainText(n *nethtml.Node) string {
	raw := extractRawText(n)
	raw = strings.ReplaceAll(raw, "\n", " ")
	raw = strings.ReplaceAll(raw, "\t", " ")
	for strings.Contains(raw, "  ") {
		raw = strings.ReplaceAll(raw, "  ", " ")
	}
	return strings.TrimSpace(raw)
}

// collapseInlineNewlines folds newlines and runs of spaces into single
// spaces so inline link text stays on one line.
func collapseInlineNewlines(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// escapeLinkText escapes stray square brackets in link text while preserving
// well-formed image (![alt](src)) and link ([text](url)) syntax that may
// already be present in the text.
func escapeLinkText(s string) string {
	if (strings.HasPrefix(s, "![") || strings.HasPrefix(s, "[")) && strings.Contains(s, "](") {
		return s
	}
	return escapeMarkdownBrackets(s)
}

func (c *Html2MdConverter) handleImage(n *nethtml.Node, buf *mdBuffer) {
	rawSrc := getAttr(n, "src")
	safeSrc := sanitizeURL(c.resolveURL(rawSrc), true)
	if safeSrc == "" {
		return
	}
	alt := escapeMarkdownBrackets(getAttr(n, "alt"))
	title := strings.ReplaceAll(getAttr(n, "title"), `"`, `\"`)

	if title != "" {
		buf.WriteString(fmt.Sprintf("![%s](%s %q)", alt, safeSrc, title))
	} else {
		buf.WriteString(fmt.Sprintf("![%s](%s)", alt, safeSrc))
	}
}

func escapeMarkdownBrackets(s string) string {
	s = strings.ReplaceAll(s, "[", "\\[")
	s = strings.ReplaceAll(s, "]", "\\]")
	return s
}

func sanitizeURL(raw string, allowDataImage bool) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	clean := ctrlCharsRegex.ReplaceAllString(raw, "")
	cleanLower := strings.ToLower(clean)

	if strings.HasPrefix(cleanLower, "javascript:") || strings.HasPrefix(cleanLower, "vbscript:") {
		return ""
	}

	if strings.HasPrefix(cleanLower, "data:") {
		if allowDataImage && (strings.HasPrefix(cleanLower, "data:image/png") ||
			strings.HasPrefix(cleanLower, "data:image/jpeg") ||
			strings.HasPrefix(cleanLower, "data:image/gif") ||
			strings.HasPrefix(cleanLower, "data:image/webp") ||
			strings.HasPrefix(cleanLower, "data:image/avif")) {
			return clean
		}
		return ""
	}

	u, err := url.Parse(clean)
	if err != nil {
		return ""
	}

	if u.Scheme != "" && !safeSchemes[strings.ToLower(u.Scheme)] {
		return ""
	}

	res := u.String()
	res = strings.ReplaceAll(res, " ", "%20")
	res = strings.ReplaceAll(res, "(", "%28")
	res = strings.ReplaceAll(res, ")", "%29")
	return res
}

func (c *Html2MdConverter) resolveURL(raw string) string {
	if raw == "" || c.baseURL == nil {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		c.warnings.addMsg(fmt.Sprintf("Bad URL: %q", raw))
		return raw
	}
	if u.IsAbs() {
		return raw
	}
	return c.baseURL.ResolveReference(u).String()
}

// -------------------------------------------------------------
// 节点清洗与属性工具辅助
// -------------------------------------------------------------

func isIgnoredNode(n *nethtml.Node, ctx *walkContext) bool {
	if n == nil {
		return true
	}
	if n.Type == nethtml.CommentNode {
		return true
	}
	if n.Type != nethtml.ElementNode {
		return false
	}
	tag := strings.ToLower(n.Data)
	switch tag {
	case "script", "style", "noscript", "template", "head":
		return true
	}

	if tag == "svg" && !hasAttr(n, "aria-label") && !hasAttr(n, "title") {
		return true
	}

	if tag == "button" {
		class := strings.ToLower(getAttr(n, "class"))
		ariaLabel := strings.ToLower(getAttr(n, "aria-label"))
		if strings.Contains(class, "copy") || strings.Contains(ariaLabel, "copy") ||
			strings.Contains(class, "toggle") || strings.Contains(ariaLabel, "close") {
			return true
		}
	}

	class := strings.ToLower(getAttr(n, "class"))
	if strings.Contains(class, "katex-html") {
		return true
	}

	if hasAttr(n, "hidden") || getAttr(n, "aria-hidden") == "true" {
		return true
	}

	style := strings.ToLower(getAttr(n, "style"))
	if strings.Contains(style, "display:none") || strings.Contains(style, "display: none") {
		return true
	}
	return false
}

func isHeadingAnchor(n *nethtml.Node) bool {
	if n.Type != nethtml.ElementNode || strings.ToLower(n.Data) != "a" {
		return false
	}
	class := strings.ToLower(getAttr(n, "class"))
	if headingAnchorRegex.MatchString(class) {
		return true
	}
	txt := strings.TrimSpace(extractRawText(n))
	return txt == "#" || txt == "¶" || txt == "§"
}

func isLeafOrShortText(n *nethtml.Node) bool {
	txt := strings.TrimSpace(extractRawText(n))
	return utf8.RuneCountInString(txt) < 3
}

func getAttr(n *nethtml.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *nethtml.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func extractRawText(n *nethtml.Node) string {
	var sb strings.Builder
	var walk func(*nethtml.Node)
	walk = func(curr *nethtml.Node) {
		if curr == nil {
			return
		}
		if curr.Type == nethtml.TextNode {
			sb.WriteString(curr.Data)
		}
		for c := curr.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

func getTableAlign(n *nethtml.Node) string {
	if a := strings.ToLower(getAttr(n, "align")); a != "" {
		return a
	}
	style := strings.ToLower(getAttr(n, "style"))
	if strings.Contains(style, "text-align:center") || strings.Contains(style, "text-align: center") {
		return "center"
	}
	if strings.Contains(style, "text-align:right") || strings.Contains(style, "text-align: right") {
		return "right"
	}
	if strings.Contains(style, "text-align:left") || strings.Contains(style, "text-align: left") {
		return "left"
	}
	return ""
}

func findCheckboxOrTask(n *nethtml.Node) (*nethtml.Node, bool, bool) {
	if hasAttr(n, "data-checked") {
		val := strings.ToLower(getAttr(n, "data-checked"))
		return nil, true, val == "true" || val == "yes"
	}

	var checkNode *nethtml.Node
	var hasCheck, isChecked bool

	var search func(*nethtml.Node)
	search = func(curr *nethtml.Node) {
		if hasCheck || curr == nil {
			return
		}
		if curr.Type == nethtml.ElementNode {
			tag := strings.ToLower(curr.Data)
			if tag == "input" && strings.EqualFold(getAttr(curr, "type"), "checkbox") {
				checkNode = curr
				hasCheck = true
				isChecked = hasAttr(curr, "checked")
				return
			}
			if role := strings.ToLower(getAttr(curr, "role")); role == "checkbox" {
				checkNode = curr
				hasCheck = true
				isChecked = getAttr(curr, "aria-checked") == "true"
				return
			}
		}
		for c := curr.FirstChild; c != nil; c = c.NextSibling {
			search(c)
		}
	}
	search(n)
	return checkNode, hasCheck, isChecked
}

// wrapFragmentIfRequired 处理未闭合或孤立的 HTML 片段，保证 net/html 解析树规范
func wrapFragmentIfRequired(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)

	for strings.HasPrefix(lower, "<!--") {
		idx := strings.Index(lower, "-->")
		if idx == -1 {
			break
		}
		trimmed = strings.TrimSpace(trimmed[idx+3:])
		lower = strings.ToLower(trimmed)
	}

	if strings.HasPrefix(lower, "<tr") {
		return "<table><tbody>" + raw + "</tbody></table>", "tr"
	}
	if strings.HasPrefix(lower, "<td") || strings.HasPrefix(lower, "<th") {
		return "<table><tbody><tr>" + raw + "</tr></tbody></table>", "td"
	}
	if strings.HasPrefix(lower, "<thead") || strings.HasPrefix(lower, "<tbody") || strings.HasPrefix(lower, "<tfoot") {
		return "<table>" + raw + "</table>", "table-section"
	}
	if strings.HasPrefix(lower, "<li") {
		return "<ul>" + raw + "</ul>", "li"
	}
	if strings.HasPrefix(lower, "<dd") || strings.HasPrefix(lower, "<dt") {
		return "<dl>" + raw + "</dl>", "dl"
	}
	return raw, ""
}
