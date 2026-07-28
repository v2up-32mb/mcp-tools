package golangx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/example/mcp-tools/internal/config"
	"github.com/example/mcp-tools/internal/mcp"
	"github.com/example/mcp-tools/internal/numconv"
	"github.com/example/mcp-tools/internal/security"
	"github.com/example/mcp-tools/internal/util"
)

type tool struct {
	name     string
	desc     string
	schema   map[string]any
	readOnly bool
	call     func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error)
}

func (t tool) Name() string           { return t.name }
func (t tool) Description() string    { return t.desc }
func (t tool) Schema() map[string]any { return t.schema }
func (t tool) ReadOnly() bool         { return t.readOnly }
func (t tool) Call(ctx context.Context, callCtx mcp.CallContext, args map[string]any) (mcp.Result, error) {
	return t.call(ctx, callCtx, args)
}

type packageContext struct {
	rootPackage *packages.Package
	allPackages []*packages.Package
	targetPkg   *packages.Package
	targetFile  *ast.File
	fset        *token.FileSet
	path        string
}

type symbolEntry struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Receiver    string `json:"receiver,omitempty"`
	Exported    bool   `json:"exported"`
	StartLine   int    `json:"start_line"`
	StartColumn int    `json:"start_column"`
	EndLine     int    `json:"end_line"`
	EndColumn   int    `json:"end_column"`
}

type navFailure struct {
	Reason  string
	Path    string
	Line    int
	Column  int
	Message string
}

func NewTools(cfg config.Config) []mcp.Tool {
	return []mcp.Tool{
		tool{name: "go.list_symbols", desc: "List top-level symbols declared in one Go source file inside allowed roots. Input is a single .go file path; output includes funcs, methods, types, vars, and consts with source ranges.", schema: schemaListSymbols(), readOnly: true, call: listSymbols(cfg)},
		tool{name: "go.find_definition", desc: "Resolve the definition for the Go identifier at path + line + column. First version covers package-level declarations, methods, and imported package symbols only. Returns definition location plus a minimal symbol summary; definitions outside allowed roots are marked with in_allowed_roots=false.", schema: schemaFindDefinition(), readOnly: true, call: findDefinition(cfg)},
	}
}

func listSymbols(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolveGoPathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		pkgCtx, failure := loadGoPackageContext(ctx, path, false)
		if failure != nil {
			return mcp.Result{}, wrapNavFailure(path, failure)
		}
		symbols := collectFileSymbols(pkgCtx.targetFile, pkgCtx.fset)
		summary := fmt.Sprintf("listed %d symbol(s)", len(symbols))
		text := summary
		if len(symbols) > 0 {
			rows := make([]string, 0, len(symbols))
			for _, sym := range symbols {
				label := sym.Kind
				if sym.Receiver != "" {
					label += "(" + sym.Receiver + ")"
				}
				rows = append(rows, fmt.Sprintf("%s %s:%d:%d", label, sym.Name, sym.StartLine, sym.StartColumn))
			}
			text = strings.Join(rows, "\n")
		}
		return mcp.TextResult(text, map[string]any{
			"summary": summary,
			"path":    path,
			"package": pkgCtx.targetPkg.Name,
			"symbols": symbolsToStructured(symbols),
		}, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func findDefinition(cfg config.Config) func(context.Context, mcp.CallContext, map[string]any) (mcp.Result, error) {
	return func(ctx context.Context, _ mcp.CallContext, args map[string]any) (mcp.Result, error) {
		path, err := resolveGoPathArg(args, "path", cfg)
		if err != nil {
			return mcp.Result{}, err
		}
		line, err := intArg(args, "line")
		if err != nil {
			return mcp.Result{}, wrapNavFailure(path, &navFailure{Reason: "validation_failed", Path: path, Message: err.Error()})
		}
		if line <= 0 {
			return mcp.Result{}, wrapNavFailure(path, &navFailure{Reason: "position_out_of_bounds", Path: path, Message: "line must be a positive integer"})
		}
		column, err := intArg(args, "column")
		if err != nil {
			return mcp.Result{}, wrapNavFailure(path, &navFailure{Reason: "validation_failed", Path: path, Line: line, Message: err.Error()})
		}
		if column <= 0 {
			return mcp.Result{}, wrapNavFailure(path, &navFailure{Reason: "position_out_of_bounds", Path: path, Line: line, Message: "column must be a positive integer"})
		}

		pkgCtx, failure := loadGoPackageContext(ctx, path, true)
		if failure != nil {
			failure.Line = line
			failure.Column = column
			return mcp.Result{}, wrapNavFailure(path, failure)
		}
		pos, failure := positionForLineColumn(pkgCtx.targetFile, pkgCtx.fset, line, column)
		if failure != nil {
			failure.Path = path
			return mcp.Result{}, wrapNavFailure(path, failure)
		}
		ident, obj, failure := resolveIdentifierAtPosition(pkgCtx, pos)
		if failure != nil {
			failure.Path = path
			failure.Line = line
			failure.Column = column
			return mcp.Result{}, wrapNavFailure(path, failure)
		}
		definitionPath, defLine, defColumn, inAllowedRoots, failure := definitionLocation(cfg, pkgCtx, obj)
		if failure != nil {
			failure.Path = path
			failure.Line = line
			failure.Column = column
			return mcp.Result{}, wrapNavFailure(path, failure)
		}
		symbolName := ident.Name
		kind := symbolKindForObject(obj)
		summary := fmt.Sprintf("definition for %s at %s:%d:%d", symbolName, definitionPath, defLine, defColumn)
		structured := map[string]any{
			"summary":           summary,
			"query_path":        path,
			"query_line":        line,
			"query_column":      column,
			"symbol_name":       symbolName,
			"symbol_kind":       kind,
			"package":           packageNameForObject(obj),
			"definition_path":   definitionPath,
			"definition_line":   defLine,
			"definition_column": defColumn,
			"in_allowed_roots":  inAllowedRoots,
		}
		if sig := signatureSummary(obj); sig != "" {
			structured["signature_summary"] = sig
		}
		return mcp.TextResult(summary, structured, mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: summary}), nil
	}
}

func resolveGoPathArg(args map[string]any, key string, cfg config.Config) (string, error) {
	raw, err := requiredGoStringArg(args, key)
	if err != nil {
		return "", mcp.WrapToolErrorWithStructured(err, mcp.AuditData{Allowed: true, ResultDigest: "validation failed"}, map[string]any{"reason": "validation_failed"})
	}
	if strings.TrimSpace(raw) == "" {
		return "", mcp.WrapToolErrorWithStructured(fmt.Errorf("%s required", key), mcp.AuditData{Allowed: true, ResultDigest: "validation failed"}, map[string]any{"reason": "validation_failed", "path": raw})
	}
	candidate := raw
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cfg.StartupDirectory, candidate)
	}
	var resolved string
	if cfg.UnsafeAllowAll {
		resolved, err = security.ResolvePathUnsafe(candidate)
	} else {
		resolved, err = security.ResolvePath(candidate, cfg.AllowedRoots)
	}
	if err != nil {
		allowed := !errors.Is(err, security.ErrPathOutsideAllowedRoots)
		return "", mcp.WrapToolErrorWithStructured(fmt.Errorf("%s: %w", key, err), mcp.AuditData{TargetPath: raw, Allowed: allowed, ResultDigest: "path rejected"}, map[string]any{"reason": "path_outside_allowed_roots", "path": raw})
	}
	if filepath.Ext(resolved) != ".go" {
		return "", wrapNavFailure(resolved, &navFailure{Reason: "not_go_file", Path: resolved, Message: "path must point to a .go file"})
	}
	return resolved, nil
}

func requiredGoStringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", fmt.Errorf("%s required", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return text, nil
}

func loadGoPackageContext(ctx context.Context, resolvedPath string, needTypes bool) (*packageContext, *navFailure) {
	mode := packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax
	if needTypes {
		mode |= packages.NeedDeps | packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes
	}
	cfg := &packages.Config{
		Context: ctx,
		Mode:    mode,
		Dir:     filepath.Dir(resolvedPath),
		Tests:   false,
	}
	loaded, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, &navFailure{Reason: "package_load_failed", Path: resolvedPath, Message: err.Error()}
	}
	allPkgs := collectPackages(loaded)
	var targetPkg *packages.Package
	var targetFile *ast.File
	var fset *token.FileSet
	for _, pkg := range allPkgs {
		for idx, file := range pkg.CompiledGoFiles {
			if samePath(file, resolvedPath) {
				targetPkg = pkg
				if idx < len(pkg.Syntax) {
					targetFile = pkg.Syntax[idx]
				}
				fset = pkg.Fset
				break
			}
		}
		if targetPkg != nil {
			break
		}
	}
	if targetPkg == nil || targetFile == nil || fset == nil {
		return nil, &navFailure{Reason: "package_load_failed", Path: resolvedPath, Message: "unable to load package for file"}
	}
	if len(targetPkg.Errors) > 0 {
		return nil, classifyPackageErrors(resolvedPath, targetPkg.Errors)
	}
	if needTypes && targetPkg.TypesInfo == nil {
		return nil, &navFailure{Reason: "package_load_failed", Path: resolvedPath, Message: "type information unavailable"}
	}
	return &packageContext{rootPackage: loaded[0], allPackages: allPkgs, targetPkg: targetPkg, targetFile: targetFile, fset: fset, path: resolvedPath}, nil
}

func collectPackages(roots []*packages.Package) []*packages.Package {
	seen := make(map[*packages.Package]bool)
	out := make([]*packages.Package, 0)
	var walk func(*packages.Package)
	walk = func(pkg *packages.Package) {
		if pkg == nil || seen[pkg] {
			return
		}
		seen[pkg] = true
		out = append(out, pkg)
		for _, dep := range pkg.Imports {
			walk(dep)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

func classifyPackageErrors(path string, errs []packages.Error) *navFailure {
	for _, pkgErr := range errs {
		if pkgErr.Kind == packages.ParseError {
			return &navFailure{Reason: "parse_failed", Path: path, Message: pkgErr.Msg}
		}
	}
	return &navFailure{Reason: "package_load_failed", Path: path, Message: errs[0].Msg}
}

func collectFileSymbols(file *ast.File, fset *token.FileSet) []symbolEntry {
	entries := make([]symbolEntry, 0)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			receiver := ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				receiver = renderExpr(fset, d.Recv.List[0].Type)
			}
			entries = append(entries, symbolEntry{
				Name:        d.Name.Name,
				Kind:        kind,
				Receiver:    receiver,
				Exported:    ast.IsExported(d.Name.Name),
				StartLine:   positionLine(fset, d.Pos()),
				StartColumn: positionColumn(fset, d.Pos()),
				EndLine:     positionLine(fset, d.End()),
				EndColumn:   positionColumn(fset, d.End()),
			})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					entries = append(entries, symbolEntry{Name: s.Name.Name, Kind: "type", Exported: ast.IsExported(s.Name.Name), StartLine: positionLine(fset, s.Pos()), StartColumn: positionColumn(fset, s.Pos()), EndLine: positionLine(fset, s.End()), EndColumn: positionColumn(fset, s.End())})
				case *ast.ValueSpec:
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					for _, name := range s.Names {
						entries = append(entries, symbolEntry{Name: name.Name, Kind: kind, Exported: ast.IsExported(name.Name), StartLine: positionLine(fset, name.Pos()), StartColumn: positionColumn(fset, name.Pos()), EndLine: positionLine(fset, name.End()), EndColumn: positionColumn(fset, name.End())})
					}
				}
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].StartLine == entries[j].StartLine {
			return entries[i].StartColumn < entries[j].StartColumn
		}
		return entries[i].StartLine < entries[j].StartLine
	})
	return entries
}

func symbolsToStructured(entries []symbolEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		item := map[string]any{
			"name":         entry.Name,
			"kind":         entry.Kind,
			"exported":     entry.Exported,
			"start_line":   entry.StartLine,
			"start_column": entry.StartColumn,
			"end_line":     entry.EndLine,
			"end_column":   entry.EndColumn,
		}
		if entry.Receiver != "" {
			item["receiver"] = entry.Receiver
		}
		out = append(out, item)
	}
	return out
}

func renderExpr(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, expr); err != nil {
		return ""
	}
	return buf.String()
}

func positionLine(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Line
}

func positionColumn(fset *token.FileSet, pos token.Pos) int {
	return fset.PositionFor(pos, false).Column
}

func positionForLineColumn(file *ast.File, fset *token.FileSet, line int, column int) (token.Pos, *navFailure) {
	tokFile := fset.File(file.Pos())
	if tokFile == nil {
		return token.NoPos, &navFailure{Reason: "position_out_of_bounds", Line: line, Column: column, Message: "file positions unavailable"}
	}
	if line < 1 || line > tokFile.LineCount() {
		return token.NoPos, &navFailure{Reason: "position_out_of_bounds", Line: line, Column: column, Message: "line out of bounds"}
	}
	lineStart := tokFile.LineStart(line)
	lineStartOffset := tokFile.Offset(lineStart)
	var lineEndOffset int
	if line == tokFile.LineCount() {
		lineEndOffset = tokFile.Size()
	} else {
		nextStart := tokFile.LineStart(line + 1)
		lineEndOffset = tokFile.Offset(nextStart) - 1
	}
	offset := lineStartOffset + column - 1
	if offset < lineStartOffset || offset > lineEndOffset {
		return token.NoPos, &navFailure{Reason: "position_out_of_bounds", Line: line, Column: column, Message: "column out of bounds"}
	}
	return tokFile.Pos(offset), nil
}

func resolveIdentifierAtPosition(pkgCtx *packageContext, pos token.Pos) (*ast.Ident, types.Object, *navFailure) {
	var best *ast.Ident
	var bestSpan int
	ast.Inspect(pkgCtx.targetFile, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		if pos < ident.Pos() || pos >= ident.End() {
			return true
		}
		span := int(ident.End() - ident.Pos())
		if best == nil || span < bestSpan {
			best = ident
			bestSpan = span
		}
		return true
	})
	if best == nil {
		return nil, nil, &navFailure{Reason: "identifier_not_found", Message: "no identifier at given position"}
	}
	if obj := selectorObject(pkgCtx.targetFile, pkgCtx.targetPkg.TypesInfo, best); obj != nil {
		if !supportedObject(obj) {
			return nil, nil, &navFailure{Reason: "definition_not_resolved", Message: "identifier kind is not supported in first version"}
		}
		return best, obj, nil
	}
	obj := pkgCtx.targetPkg.TypesInfo.Uses[best]
	if obj == nil {
		obj = pkgCtx.targetPkg.TypesInfo.Defs[best]
	}
	if obj == nil {
		return nil, nil, &navFailure{Reason: "definition_not_resolved", Message: "definition could not be resolved"}
	}
	if !supportedObject(obj) {
		return nil, nil, &navFailure{Reason: "definition_not_resolved", Message: "identifier kind is not supported in first version"}
	}
	return best, obj, nil
}

func selectorObject(file *ast.File, info *types.Info, ident *ast.Ident) types.Object {
	var obj types.Object
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel != ident {
			return true
		}
		if selection := info.Selections[sel]; selection != nil {
			obj = selection.Obj()
			return false
		}
		if use := info.Uses[ident]; use != nil {
			obj = use
			return false
		}
		return true
	})
	return obj
}

func supportedObject(obj types.Object) bool {
	switch typed := obj.(type) {
	case *types.Func:
		if sig, ok := typed.Type().(*types.Signature); ok && sig.Recv() != nil {
			return true
		}
		return isPackageScopeObject(obj)
	case *types.TypeName:
		return isPackageScopeObject(obj)
	case *types.Var:
		return !typed.IsField() && isPackageScopeObject(obj)
	case *types.Const:
		return isPackageScopeObject(obj)
	default:
		return false
	}
}

func symbolKindForObject(obj types.Object) string {
	switch typed := obj.(type) {
	case *types.Func:
		if sig, ok := typed.Type().(*types.Signature); ok && sig.Recv() != nil {
			return "method"
		}
		return "func"
	case *types.TypeName:
		return "type"
	case *types.Var:
		return "var"
	case *types.Const:
		return "const"
	default:
		return ""
	}
}

func isPackageScopeObject(obj types.Object) bool {
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	parent := obj.Parent()
	return parent != nil && parent == obj.Pkg().Scope()
}

func definitionLocation(cfg config.Config, pkgCtx *packageContext, obj types.Object) (string, int, int, bool, *navFailure) {
	if obj == nil || obj.Pos() == token.NoPos {
		return "", 0, 0, false, &navFailure{Reason: "definition_not_resolved", Message: "definition position unavailable"}
	}
	for _, pkg := range pkgCtx.allPackages {
		if pkg == nil || pkg.Fset == nil {
			continue
		}
		posn := pkg.Fset.PositionFor(obj.Pos(), true)
		if posn.Filename == "" {
			continue
		}
		inAllowed := cfg.UnsafeAllowAll || pathInAllowedRoots(posn.Filename, cfg.AllowedRoots)
		return posn.Filename, posn.Line, posn.Column, inAllowed, nil
	}
	return "", 0, 0, false, &navFailure{Reason: "definition_not_resolved", Message: "definition file could not be located"}
}

func pathInAllowedRoots(path string, roots []string) bool {
	for _, root := range roots {
		if _, err := security.ResolvePath(path, []string{root}); err == nil {
			return true
		}
	}
	return false
}

func packageNameForObject(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	return obj.Pkg().Name()
}

func signatureSummary(obj types.Object) string {
	if obj == nil {
		return ""
	}
	return types.ObjectString(obj, func(other *types.Package) string {
		if other == nil {
			return ""
		}
		return other.Name()
	})
}

func wrapNavFailure(path string, failure *navFailure) error {
	message := "navigation failed"
	structured := map[string]any{"path": path}
	if failure != nil {
		if strings.TrimSpace(failure.Message) != "" {
			message = failure.Message
		}
		if failure.Reason != "" {
			structured["reason"] = failure.Reason
		}
		if failure.Path != "" {
			structured["path"] = failure.Path
		}
		if failure.Line > 0 {
			structured["line"] = failure.Line
		}
		if failure.Column > 0 {
			structured["column"] = failure.Column
		}
	}
	return mcp.WrapToolErrorWithStructured(errors.New(message), mcp.AuditData{TargetPath: path, Allowed: true, ResultDigest: "navigation failed"}, structured)
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func schemaListSymbols() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []string{"path"},
	}
}

func schemaFindDefinition() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "minLength": 1},
			"line":   map[string]any{"type": "integer", "minimum": 1},
			"column": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"path", "line", "column"},
	}
}

func intArg(args map[string]any, key string) (int, error) {
	value, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("%s required", key)
	}
	switch typed := value.(type) {
	case float64:
		parsed, err := util.ParseIntegerFloat(typed, key)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	case json.Number:
		parsed, err := numconv.IntFromJSONNumber(typed, key)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	case int:
		return typed, nil
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
}
