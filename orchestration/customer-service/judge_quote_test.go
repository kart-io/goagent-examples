package main

import (
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	mdtext "github.com/yuin/goldmark/text"
)

// quote 校验（judge-executor/5）：先逐字匹配答复原文；不通过时，只忽略经 CommonMark 解析确认的 `**…**` 加粗定界符再匹配，
// 命中位置必须映射回答复里的一段连续原文。不删正文、不改标点、不拼接片段，也不清洗其他 Markdown。

const (
	quoteExact = "exact"           // 引文逐字出现在答复中
	quoteBold  = "bold_delimiters" // 忽略答复中已解析确认的加粗定界符后出现
)

// quoteMatch 是一条非空引文的匹配依据：模型原始引文保留在 checkAnswer.Quote，这里记录它对应的答复原文区间（字节偏移，左闭右开）。
type quoteMatch struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Source string `json:"source"`
}

// boldDelimiters 返回答复中经 goldmark 解析为加粗（Emphasis level 2）且两侧定界符恰为 `**` 的定界符字节起点，按位置升序。
// 加粗首尾不是纯文本、定界符是 `__`、或与相邻 `*` 连成更长的星号串时，不算确认，保持严格匹配。
func boldDelimiters(reply string) []int {
	src := []byte(reply)
	doc := goldmark.New().Parser().Parse(mdtext.NewReader(src))
	var out []int
	isStars := func(i int) bool {
		return i >= 0 && i+2 <= len(src) && src[i] == '*' && src[i+1] == '*' &&
			(i == 0 || src[i-1] != '*') && (i+2 == len(src) || src[i+2] != '*')
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		e, ok := n.(*ast.Emphasis)
		if !entering || !ok || e.Level != 2 {
			return ast.WalkContinue, nil
		}
		first, ok1 := e.FirstChild().(*ast.Text)
		last, ok2 := e.LastChild().(*ast.Text)
		if ok1 && ok2 && isStars(first.Segment.Start-2) && isStars(last.Segment.Stop) {
			out = append(out, first.Segment.Start-2, last.Segment.Stop)
		}
		return ast.WalkContinue, nil
	})
	slices.Sort(out)
	return out
}

// matchQuote 判断引文是否为答复原文，并给出匹配依据；quote 已去掉首尾空白且非空。
func matchQuote(reply, quote string) (quoteMatch, bool) {
	if i := strings.Index(reply, quote); i >= 0 {
		return quoteMatch{Method: quoteExact, Start: i, End: i + len(quote), Source: quote}, true
	}
	delims := boldDelimiters(reply)
	if len(delims) == 0 {
		return quoteMatch{}, false
	}
	// view 是去掉已确认定界符的答复，offset[i] 是 view 第 i 个字节在原文中的位置。
	view := make([]byte, 0, len(reply))
	offset := make([]int, 0, len(reply))
	for i, d := 0, 0; i < len(reply); i++ {
		if d < len(delims) && i == delims[d] {
			i++
			d++
			continue
		}
		view = append(view, reply[i])
		offset = append(offset, i)
	}
	i := strings.Index(string(view), quote)
	if i < 0 {
		return quoteMatch{}, false
	}
	start, end := offset[i], offset[i+len(quote)-1]+1
	return quoteMatch{Method: quoteBold, Start: start, End: end, Source: reply[start:end]}, true
}
