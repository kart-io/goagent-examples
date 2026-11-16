package docs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const corpus = "../../testdata/corpus"

func mustCollect(t *testing.T) []Chunk {
	t.Helper()
	chunks, err := Collect(corpus)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return chunks
}

func TestCollectSources(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		wantInText string
	}{
		{"markdown 小节", "goagent-contrib:durable/pgstore/README.md#建表", "执行 `Schema`"},
		{"godoc", "goagent:memory#godoc", "Package memory 提供智能体的长期记忆能力"},
		{"godoc 含导出函数注释", "goagent:memory#godoc", "向指定命名空间写入一条记忆"},
	}
	chunks := mustCollect(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := false
			for _, c := range chunks {
				if c.Source != tt.source {
					continue
				}
				found = true
				if !strings.HasPrefix(c.Text, "来源："+tt.source+"\n") {
					t.Errorf("块文本缺少来源首行: %q", c.Text)
				}
				if strings.Contains(c.Text, tt.wantInText) {
					return
				}
			}
			if !found {
				t.Fatalf("未找到 Source=%q 的块", tt.source)
			}
			t.Errorf("没有块包含 %q", tt.wantInText)
		})
	}
}

func TestCollectSkipsUnexported(t *testing.T) {
	for _, c := range mustCollect(t) {
		if strings.Contains(c.Text, "internalHelper") {
			t.Fatalf("未导出符号不应进入索引: %q", c.Text)
		}
	}
}

func TestCollectChunkSize(t *testing.T) {
	for _, c := range mustCollect(t) {
		_, body, ok := strings.Cut(c.Text, "\n")
		if !ok {
			t.Fatalf("块文本缺少来源行: %q", c.Text)
		}
		if n := utf8.RuneCountInString(body); n > 800 {
			t.Errorf("%s 正文 %d 个字符，超过 800", c.Source, n)
		}
	}
}

func TestCollectStableIDs(t *testing.T) {
	ids := func() []string {
		var out []string
		for _, c := range mustCollect(t) {
			out = append(out, c.ID)
		}
		return out
	}
	a, b := ids(), ids()
	if len(a) == 0 {
		t.Fatal("没有采集到任何块")
	}
	seen := map[string]bool{}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 块 ID 不稳定: %s vs %s", i, a[i], b[i])
		}
		if seen[a[i]] {
			t.Fatalf("ID 重复: %s", a[i])
		}
		seen[a[i]] = true
	}
}
