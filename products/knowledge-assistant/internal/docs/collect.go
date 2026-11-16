// Package docs 负责把仓库文档切块并写入 Qdrant 索引。
package docs

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kart-io/goagent-contrib/document"
)

// Chunk 是一个可检索的文本块。
type Chunk struct {
	// ID 是由 Source 与块序号派生的 UUIDv5，Qdrant 点 ID 只接受 UUID 或整数。
	ID string
	// Source 形如 `仓库名:仓库相对路径#小节`。
	Source string
	// Text 第一行固定为 `来源：<Source>`，因为框架送给模型的证据不含元数据。
	Text string
}

var skipDirs = map[string]bool{".git": true, "vendor": true, "testdata": true, "node_modules": true}

// skipPaths 按仓库相对路径排除目录。docs/superpowers 下的 specs 与 plans 都带日期，描述当时的意图而非现状，
// 真实嵌入下却排在模块文档前面（见 verification 中的首次真实验收）。代价是丢了只写在设计里的细节，
// 例如 durable/pgstore 的具体建表 SQL（只在 specs 和 schema.go 的常量值里，godoc 不含常量值）。
var skipPaths = map[string]bool{"docs/superpowers": true}

var headingRe = regexp.MustCompile(`^#{1,4}\s+(.+?)\s*$`)

// Collect 采集 root 下各仓库（一级子目录）的 Markdown 小节与 Go 包文档。
func Collect(root string) ([]Chunk, error) {
	repos, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("读取语料根目录 %s: %w", root, err)
	}
	splitter := document.NewRecursiveCharacterTextSplitter(document.RecursiveCharacterTextSplitterConfig{ChunkSize: 800, ChunkOverlap: 100})
	c := &collector{splitter: splitter, next: map[string]int{}}
	for _, r := range repos {
		if !r.IsDir() || skipDirs[r.Name()] {
			continue
		}
		if err := c.collectRepo(root, r.Name()); err != nil {
			return nil, err
		}
	}
	return c.chunks, nil
}

type collector struct {
	splitter *document.RecursiveCharacterTextSplitter
	next     map[string]int // 每个 Source 已分配的块序号
	chunks   []Chunk
}

func (c *collector) collectRepo(root, repo string) error {
	repoDir := filepath.Join(root, repo)
	goDirs := map[string][]string{}
	err := filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path != repoDir && (skipDirs[d.Name()] || skipPaths[rel]) {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, ".md"):
			return c.collectMarkdown(repo, rel, path)
		case strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"):
			dir := filepath.ToSlash(filepath.Dir(rel))
			goDirs[dir] = append(goDirs[dir], path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("遍历仓库 %s: %w", repo, err)
	}
	dirs := make([]string, 0, len(goDirs))
	for d := range goDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if err := c.collectGodoc(repo, d, goDirs[d]); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) collectMarkdown(repo, rel, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", path, err)
	}
	defer f.Close()

	// 围栏代码块里的 `# 注释` 不是标题。
	section, inFence := "概述", false
	var body []string
	flush := func() error {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		body = nil
		if text == "" {
			return nil
		}
		return c.add(fmt.Sprintf("%s:%s#%s", repo, rel, section), text)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if m := headingRe.FindStringSubmatch(line); m != nil && !inFence {
			if err := flush(); err != nil {
				return err
			}
			section = m[1]
		}
		body = append(body, line)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取 %s: %w", path, err)
	}
	return flush()
}

func (c *collector) collectGodoc(repo, dir string, files []string) error {
	fset := token.NewFileSet()
	byPkg := map[string][]*ast.File{}
	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("解析 %s: %w", p, err)
		}
		byPkg[f.Name.Name] = append(byPkg[f.Name.Name], f)
	}
	names := make([]string, 0, len(byPkg))
	for n := range byPkg {
		names = append(names, n)
	}
	sort.Strings(names)

	var parts []string
	for _, n := range names {
		pkg, err := doc.NewFromFiles(fset, byPkg[n], repo+"/"+dir)
		if err != nil {
			return fmt.Errorf("提取 %s 的包文档: %w", dir, err)
		}
		parts = append(parts, pkgText(pkg))
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return nil
	}
	return c.add(fmt.Sprintf("%s:%s#godoc", repo, dir), text)
}

// pkgText 拼接包注释与导出符号（含导出结构体字段）的注释；doc 默认已过滤未导出符号。
func pkgText(p *doc.Package) string {
	var out []string
	if d := strings.TrimSpace(p.Doc); d != "" {
		out = append(out, "Package "+p.Name+"\n"+d)
	}
	sym := func(name, d string) {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, name+"\n"+d)
		}
	}
	for _, v := range append(append([]*doc.Value{}, p.Consts...), p.Vars...) {
		sym(strings.Join(v.Names, ", "), v.Doc)
	}
	for _, f := range p.Funcs {
		sym(f.Name, f.Doc)
	}
	for _, t := range p.Types {
		sym(t.Name, t.Doc)
		// 默认值、单位这类约定常写在字段注释上，漏掉它们模型只能从过期的计划文档里猜。
		for _, f := range structFields(t.Decl) {
			if f.Doc == nil {
				continue
			}
			for _, n := range f.Names {
				if n.IsExported() {
					sym(t.Name+"."+n.Name, f.Doc.Text())
				}
			}
		}
		for _, f := range t.Funcs {
			sym(f.Name, f.Doc)
		}
		for _, m := range t.Methods {
			sym(t.Name+"."+m.Name, m.Doc)
		}
	}
	return strings.Join(out, "\n\n")
}

func structFields(decl *ast.GenDecl) []*ast.Field {
	var out []*ast.Field
	for _, spec := range decl.Specs {
		if ts, ok := spec.(*ast.TypeSpec); ok {
			if st, ok := ts.Type.(*ast.StructType); ok {
				out = append(out, st.Fields.List...)
			}
		}
	}
	return out
}

func (c *collector) add(source, text string) error {
	parts, err := c.splitter.SplitText(text)
	if err != nil {
		return fmt.Errorf("切分 %s: %w", source, err)
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n := c.next[source]
		c.next[source]++
		c.chunks = append(c.chunks, Chunk{
			ID:     uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("ka:chunk:%s#%d", source, n))).String(),
			Source: source,
			Text:   "来源：" + source + "\n" + p,
		})
	}
	return nil
}
