// Package formatcheck 检查示例的数据格式转换都经由 github.com/kart-io/goagent/codec 及其子包（codec/json、codec/yaml），规则与 goagent 的 apicheck 一致。
package formatcheck

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// formatLibs 是只允许 codec 导入的格式库：JSON/YAML/TOML/XML 的标准库与常见第三方实现，以及 mapstructure。
var formatLibs = []string{
	"encoding/json", "encoding/xml",
	"go.yaml.in/yaml/", "gopkg.in/yaml.", "sigs.k8s.io/yaml", "github.com/goccy/go-yaml",
	"github.com/go-viper/mapstructure", "github.com/mitchellh/mapstructure",
	"github.com/bytedance/sonic", "github.com/json-iterator/go", "github.com/goccy/go-json", "github.com/mailru/easyjson",
	"github.com/tidwall/gjson", "github.com/tidwall/sjson", "github.com/buger/jsonparser",
	"github.com/pelletier/go-toml", "github.com/BurntSushi/toml",
}

func formatLib(path string) bool {
	for _, lib := range formatLibs {
		if path == lib || strings.HasPrefix(path, strings.TrimSuffix(lib, "/")+"/") || strings.HasSuffix(lib, ".") && strings.HasPrefix(path, lib) {
			return true
		}
	}
	return false
}

// 含独立 go.mod 的子模块（如固定旧版 goagent 的 httpagent）不在本模块内，升级 goagent 后再纳入。
func TestFormatConversionGoesThroughCodec(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); path != root && err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		node, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range node.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if formatLib(p) {
				t.Errorf("%s: 数据格式转换须经 github.com/kart-io/goagent/codec 及其子包，不得直接导入 %s", fset.Position(imp.Pos()), p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFormatLibMatching(t *testing.T) {
	for path, want := range map[string]bool{
		"encoding/json": true, "encoding/json/v2": true, "encoding/json/jsontext": true, "encoding/xml": true,
		"go.yaml.in/yaml/v3": true, "gopkg.in/yaml.v3": true, "github.com/go-viper/mapstructure/v2": true,
		"github.com/kart-io/goagent/codec": false, "github.com/kart-io/goagent/codec/json": false, "encoding/csv": false, "encoding/hex": false, "encoding/jsonx": false,
	} {
		if got := formatLib(path); got != want {
			t.Errorf("formatLib(%q) = %v，期望 %v", path, got, want)
		}
	}
}
