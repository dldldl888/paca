// Parse only the named Go test body; comments and strings cannot satisfy a binding.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		panic("expected file, test and SUT")
	}
	file, err := parser.ParseFile(token.NewFileSet(), args[0], nil, 0)
	if err != nil {
		panic(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != args[1] || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil {
			continue
		}
		// Count invoked closures and t.Run callbacks, not merely declared helpers.
		invoked := make(map[*ast.FuncLit]bool)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if closure, ok := call.Fun.(*ast.FuncLit); ok {
				invoked[closure] = true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Run" {
				for _, argument := range call.Args {
					if closure, ok := argument.(*ast.FuncLit); ok {
						invoked[closure] = true
					}
				}
			}
			return true
		})
		callsSUT, asserts := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if closure, ok := n.(*ast.FuncLit); ok && !invoked[closure] {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch x := call.Fun.(type) {
			case *ast.Ident:
				name = x.Name
			case *ast.SelectorExpr:
				name = x.Sel.Name
			}
			callsSUT = callsSUT || name == args[2]
			asserts = asserts || name == "Fatal" || name == "Fatalf" || name == "Error" || name == "Errorf" || name == "Equal" || name == "NoError" || name == "True" || name == "False"
			return true
		})
		if callsSUT && asserts {
			return
		}
		fmt.Fprintln(os.Stderr, "test body must invoke SUT and assert behavior")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "missing named Go test body")
	os.Exit(1)
}
