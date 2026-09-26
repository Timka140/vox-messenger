package logsafe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoPrivateLogAttrs — сторож: в коде приложения slog-атрибут с ключом из
// blockedKeys получает только константу, а ключ собеседника — не ник.
// Обёртка всё равно вырежет, но место вызова должно писать id/длину сразу.
func TestNoPrivateLogAttrs(t *testing.T) {
	var files []string
	_ = filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	files = append(files, filepath.Join("..", "..", "main.go"))

	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "slog" || (sel.Sel.Name != "String" && sel.Sel.Name != "Any") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			key, _ := strconv.Unquote(lit.Value)
			_, constVal := call.Args[1].(*ast.BasicLit)
			val := strings.ToLower(exprText(call.Args[1]))
			switch {
			case blockedKeys[key] && !constVal:
				t.Errorf("%s: slog %q пишет значение — только длину или id", fset.Position(call.Pos()), key)
			case key == "name" && !constVal:
				t.Errorf("%s: slog \"name\" пишет имя файла или группы — нужен id", fset.Position(call.Pos()))
			case key == "nick_tag" || key == "source":
				// свой nick#tag — подпись самого лога, по ней сервер раскладывает файлы
			case strings.Contains(val, "nick") || strings.Contains(val, "groupname") || strings.HasSuffix(val, ".name"):
				t.Errorf("%s: slog %q пишет ник или название — нужен короткий id", fset.Position(call.Pos()), key)
			}
			return true
		})
	}
}

func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		s := exprText(x.Fun) + "("
		for _, a := range x.Args {
			s += exprText(a) + ","
		}
		return s + ")"
	case *ast.IndexExpr:
		return exprText(x.X)
	}
	return ""
}
