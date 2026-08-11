package agentclient

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

// protoPkg 是生成代码的 import 路径。
const protoPkg = "github.com/Jepson-Wang/mini_agent_backend/gen/agent/v1"

// allowedDir 是唯一允许 import protoPkg 的目录(相对仓库根)。
const allowedDir = "internal/agentclient"

// TestOnlyAgentClientImportsProto 保证 protobuf 生成的类型不越过这个包。
//
// 这条规矩光写在注释里守不住:handler 里随手拼一个 *agentv1.CancelTaskRequest
// 是能编过的,而且看起来很自然 —— 直到 Agent 那边改一次字段名,
// handler、service 和它们的测试一起红。历史上就漏过一次,所以拿测试盯着。
//
// 例外只有 agentclient 自己:proto 类型进出的翻译在那里做
// (toEvent、CancelTaskReq → agentv1.CancelTaskRequest)。
func TestOnlyAgentClientImportsProto(t *testing.T) {
	root := repoRoot(t)

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// gen 是生成代码自己,.git 和 bin 里没有要看的东西。
			switch d.Name() {
			case ".git", "gen", "bin":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, allowedDir+"/") {
			return nil
		}

		// ImportsOnly:只解析到 import 块就停,不用把整个文件建成 AST。
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if p == protoPkg {
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库: %v", err)
	}

	for _, f := range offenders {
		t.Errorf("%s 直接 import 了 %s —— 只有 %s 可以。\n"+
			"需要什么就在 agentclient 里加一个本地类型(照 Event / CancelTaskReq 的样子),"+
			"别把 wire 类型顶到业务层。", f, protoPkg, allowedDir)
	}
}

// 这条防的是上面那条测试自己失效:哪天有人改了模块路径或者把 gen 挪了地方,
// protoPkg 就再也匹配不到任何东西,测试会一直绿着,但它什么也没在保护。
func TestProtoPkgIsActuallyImportedHere(t *testing.T) {
	root := repoRoot(t)

	f, err := parser.ParseFile(token.NewFileSet(),
		filepath.Join(root, allowedDir, "client.go"), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("解析 client.go: %v", err)
	}
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == protoPkg {
			return
		}
	}
	t.Fatalf("client.go 没有 import %s —— 常量过期了,上面那条测试其实什么都没在查", protoPkg)
}

// repoRoot 从当前目录往上找 go.mod。测试的工作目录是包所在目录,不是仓库根。
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("往上找不到 go.mod")
		}
		dir = parent
	}
}
