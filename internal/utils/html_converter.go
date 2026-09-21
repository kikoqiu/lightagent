package utils

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	nethtml "golang.org/x/net/html"
)

// Html2MdConvertResult 转换结果与诊断信息
type Html2MdConvertResult struct {
	Markdown string   // 转换后的 Markdown 文本
	Warnings []string // 聚合的诊断与警告信息
}

// Html2MdOptions 转换配置选项
type Html2MdOptions struct {
	BaseURL         string // 基准 URL，用于补全相对路径
	BulletMarker    string // 无序列表项目符号（默认 "-"）
	CodeFence       string // 代码块围栏字符（默认 "```"）
	PreserveDetails bool   // 是否保留 <details>/<summary> 原始 HTML 标签
}

// Html2MdOptionFunc 配置函数闭包
type Html2MdOptionFunc func(*Html2MdOptions)

func WithBaseURL(u string) Html2MdOptionFunc { return func(o *Html2MdOptions) { o.BaseURL = u } }
func WithBulletMarker(m string) Html2MdOptionFunc {
	return func(o *Html2MdOptions) { o.BulletMarker = m }
}
func WithCodeFence(f string) Html2MdOptionFunc { return func(o *Html2MdOptions) { o.CodeFence = f } }
func WithPreserveDetails(p bool) Html2MdOptionFunc {
	return func(o *Html2MdOptions) { o.PreserveDetails = p }
}

func defaultHtml2MdOptions() Html2MdOptions {
	return Html2MdOptions{
		BulletMarker:    "-",
		CodeFence:       "```",
		PreserveDetails: true,
	}
}

// Html2MdConverter 核心转换器上下文结构
type Html2MdConverter struct {
	opts     Html2MdOptions
	baseURL  *url.URL
	warnings *warningTracker
}

// walkContext 树遍历上下文
type walkContext struct {
	depth          int
	inPre          bool
	inHeading      bool
	atListItemHead bool // 用于解决列表项内首个段落导致 bullet 悬空的问题
	listDepth      int
	isOrdered      bool
	listCounter    int
}

// warningTracker 告警与诊断跟踪器
type warningTracker struct {
	tagCounts map[string]int
	messages  map[string]int
}

func newWarningTracker() *warningTracker {
	return &warningTracker{
		tagCounts: make(map[string]int),
		messages:  make(map[string]int),
	}
}

func (w *warningTracker) addTag(tag string) {
	w.tagCounts[strings.ToLower(tag)]++
}

func (w *warningTracker) addMsg(msg string) {
	w.messages[msg]++
}

func (w *warningTracker) summary() []string {
	var res []string
	for tag, count := range w.tagCounts {
		if count == 1 {
			res = append(res, fmt.Sprintf("Unknown <%s>", tag))
		} else {
			res = append(res, fmt.Sprintf("Unknown <%s> x%d", tag, count))
		}
	}
	for msg, count := range w.messages {
		if count == 1 {
			res = append(res, msg)
		} else {
			res = append(res, fmt.Sprintf("%s x%d", msg, count))
		}
	}
	sort.Strings(res)
	return res
}

// Html2MdConvert 字符串转换公共入口
func Html2MdConvert(htmlStr string, opts ...Html2MdOptionFunc) (res Html2MdConvertResult, err error) {
	return Html2MdConvertReader(strings.NewReader(htmlStr), opts...)
}

// Html2MdConvertReader 核心流式转换公共入口
func Html2MdConvertReader(r io.Reader, opts ...Html2MdOptionFunc) (res Html2MdConvertResult, err error) {
	cfg := defaultHtml2MdOptions()
	for _, opt := range opts {
		opt(&cfg)
	}

	tracker := newWarningTracker()
	var parsedBase *url.URL
	if cfg.BaseURL != "" {
		if u, parseErr := url.Parse(cfg.BaseURL); parseErr == nil {
			parsedBase = u
		} else {
			tracker.addMsg(fmt.Sprintf("Invalid BaseURL: %s", cfg.BaseURL))
		}
	}

	c := &Html2MdConverter{
		opts:     cfg,
		baseURL:  parsedBase,
		warnings: tracker,
	}

	// 顶级 Failsafe 容灾兜底
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic recovered: %v", rec)
		}
	}()

	rawBytes, readErr := io.ReadAll(r)
	if readErr != nil {
		return Html2MdConvertResult{}, readErr
	}

	// 动态 innerHTML 片段智能包裹
	preparedHTML, wrappedType := wrapFragmentIfRequired(string(rawBytes))
	if wrappedType != "" {
		tracker.addMsg(fmt.Sprintf("Fragment <%s> wrapped", wrappedType))
	}

	doc, parseErr := nethtml.Parse(strings.NewReader(preparedHTML))
	if parseErr != nil {
		return Html2MdConvertResult{}, fmt.Errorf("HTML parsing failed: %w", parseErr)
	}

	buf := newMdBuffer()
	ctx := &walkContext{depth: 0}

	c.walk(doc, buf, ctx)

	res.Markdown = strings.TrimSpace(buf.String())
	res.Warnings = tracker.summary()
	return res, nil
}
