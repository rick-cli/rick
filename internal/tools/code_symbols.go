package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// CodeSymbolsTool provides LSP-like code navigation: go-to-definition, find references,
// list symbols in a file. Uses Go's parser — no external LSP server needed for Go files.
type CodeSymbolsTool struct{}

func (CodeSymbolsTool) Name() string { return "code_symbols" }

func (CodeSymbolsTool) ReadOnly() bool { return true }

func (CodeSymbolsTool) Description() string {
	return "Navigate symbols in Go files: list declarations, find a definition, or find references. Use grep for other files."
}

func (CodeSymbolsTool) Schema() map[string]any {
	return obj(map[string]any{
		"action": enumProp("What to do.", "symbols", "definition", "references"),
		"path":   pathProp("Go source file path (absolute or relative to project root)."),
		"symbol": strProp("Symbol name to look up (required for definition/references)."),
	}, "action", "path")
}

type codeSymbolsArgs struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	Symbol string `json:"symbol"`
}

func (t CodeSymbolsTool) Run(_ context.Context, tc Context, in json.RawMessage) (Result, error) {
	var a codeSymbolsArgs
	if err := RepairDecode(in, &a, t.Schema(), tc.Repair); err != nil {
		return Errf("invalid arguments: %v", err), nil
	}
	if a.Path == "" {
		return Errf("path is required"), nil
	}
	p := resolvePath(tc.Cwd, a.Path)
	if filepath.Ext(p) != ".go" {
		return Errf("code_symbols only supports .go files"), nil
	}
	if _, err := os.Stat(p); err != nil {
		return Errf("file not found: %s", relTo(tc.Cwd, p)), nil
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, nil, parser.AllErrors)
	if err != nil {
		return Errf("parse error: %v", err), nil
	}

	switch a.Action {
	case "symbols":
		res, err := listSymbols(fset, f, p, tc.Cwd)
		return repairNote(res, noteOf(tc)), err
	case "definition":
		res, err := findDefinition(fset, f, a.Symbol, p, tc.Cwd)
		return repairNote(res, noteOf(tc)), err
	case "references":
		res, err := findReferences(fset, f, a.Symbol, p, tc.Cwd)
		return repairNote(res, noteOf(tc)), err
	default:
		return Errf("unknown action %q (symbols | definition | references)", a.Action), nil
	}
}

// noteOf reads the per-call repair note threaded through Context.
func noteOf(tc Context) string {
	if tc.Repair != nil && tc.Repair.Note != nil {
		return *tc.Repair.Note
	}
	return ""
}

func listSymbols(fset *token.FileSet, f *ast.File, path, cwd string) (Result, error) {
	var b strings.Builder
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			pos := fset.Position(d.Pos())
			recv := ""
			if d.Recv != nil {
				recv = "(" + fieldListStr(d.Recv) + ") "
			}
			fmt.Fprintf(&b, "func %s%s  :%d\n", recv, d.Name.Name, pos.Line)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(s.Pos())
					fmt.Fprintf(&b, "type %s  :%d\n", s.Name.Name, pos.Line)
				case *ast.ValueSpec:
					pos := fset.Position(s.Pos())
					for _, n := range s.Names {
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						fmt.Fprintf(&b, "%s %s  :%d\n", kind, n.Name, pos.Line)
					}
				}
			}
		}
	}
	if b.Len() == 0 {
		return Result{Output: "(no top-level symbols found)", Title: relTo(cwd, path)}, nil
	}
	return Result{Output: capCodeSymbolsOutput(strings.TrimRight(b.String(), "\n")), Title: relTo(cwd, path)}, nil
}

func findDefinition(fset *token.FileSet, f *ast.File, symbol, path, cwd string) (Result, error) {
	if symbol == "" {
		return Errf("symbol is required for definition lookup"), nil
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == symbol {
				pos := fset.Position(d.Pos())
				return Result{Output: fmt.Sprintf("%s:%d — func %s", relTo(cwd, path), pos.Line, symbol), Title: symbol}, nil
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name == symbol {
						pos := fset.Position(s.Pos())
						return Result{Output: fmt.Sprintf("%s:%d — type %s", relTo(cwd, path), pos.Line, symbol), Title: symbol}, nil
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name == symbol {
							pos := fset.Position(n.Pos())
							return Result{Output: fmt.Sprintf("%s:%d — %s", relTo(cwd, path), pos.Line, symbol), Title: symbol}, nil
						}
					}
				}
			}
		}
	}
	return Errf("symbol %q not defined in %s", symbol, relTo(cwd, path)), nil
}

func findReferences(fset *token.FileSet, f *ast.File, symbol, path, cwd string) (Result, error) {
	if symbol == "" {
		return Errf("symbol is required for references lookup"), nil
	}
	var refs []string
	ast.Inspect(f, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok || ident.Name != symbol {
			return true
		}
		pos := fset.Position(ident.Pos())
		refs = append(refs, fmt.Sprintf(":%d", pos.Line))
		return true
	})
	if len(refs) == 0 {
		return Result{Output: fmt.Sprintf("no references to %q in %s", symbol, relTo(cwd, path)), Title: symbol}, nil
	}
	return Result{Output: capCodeSymbolsOutput(fmt.Sprintf("%s — %s", relTo(cwd, path), strings.Join(refs, ", "))), Title: symbol}, nil
}

const maxCodeSymbolsOutputBytes = 16 << 10

func capCodeSymbolsOutput(output string) string {
	if len(output) <= maxCodeSymbolsOutputBytes {
		return output
	}
	suffix := fmt.Sprintf("\n… <symbols output truncated at %d bytes>", maxCodeSymbolsOutputBytes)
	limit := maxCodeSymbolsOutputBytes - len(suffix)
	for limit > 0 && !utf8.RuneStart(output[limit]) {
		limit--
	}
	if line := strings.LastIndex(output[:limit], "\n"); line > 0 {
		limit = line
	}
	return output[:limit] + suffix
}

func fieldListStr(fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		for _, n := range f.Names {
			parts = append(parts, n.Name)
		}
	}
	return strings.Join(parts, ", ")
}
