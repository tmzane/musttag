// Package musttag implements the musttag analyzer.
package musttag

import (
	"flag"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

// Func describes a function call to look for, e.g. json.Marshal.
type Func struct {
	Name   string // Name is the full name of the function, including the package.
	Tag    string // Tag is the struct tag whose presence should be ensured.
	ArgPos int    // ArgPos is the position of the argument to check.
}

// New creates a new musttag analyzer.
// To report a custom function provide its description via Func,
// it will be added to the builtin ones.
func New(funcs ...Func) *analysis.Analyzer {
	var flagFuncs []Func
	return &analysis.Analyzer{
		Name:     "musttag",
		Doc:      "enforce field tags in (un)marshaled structs",
		Flags:    flags(&flagFuncs),
		Requires: []*analysis.Analyzer{inspect.Analyzer},
		Run: func(pass *analysis.Pass) (any, error) {
			l := len(builtin) + len(funcs) + len(flagFuncs)
			m := make(map[string]Func, l)
			toMap := func(slice []Func) {
				for _, fn := range slice {
					m[fn.Name] = fn
				}
			}
			toMap(builtin)
			toMap(funcs)
			toMap(flagFuncs)
			return run(pass, m)
		},
	}
}

// flags creates a flag set for the analyzer.
// The funcs slice will be filled with custom functions passed via CLI flags.
func flags(funcs *[]Func) flag.FlagSet {
	fs := flag.NewFlagSet("musttag", flag.ContinueOnError)
	fs.Func("fn", "report custom function (name:tag:argpos)", func(s string) error {
		parts := strings.Split(s, ":")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			return strconv.ErrSyntax
		}
		pos, err := strconv.Atoi(parts[2])
		if err != nil {
			return err
		}
		*funcs = append(*funcs, Func{
			Name:   parts[0],
			Tag:    parts[1],
			ArgPos: pos,
		})
		return nil
	})
	return *fs
}

// for tests only.
var (
	// should the same struct be reported only once for the same tag?
	reportOnce = true

	// reportf is a wrapper for pass.Reportf (as a variable, so it could be mocked in tests).
	reportf = func(pass *analysis.Pass, pos token.Pos, fn Func) {
		// TODO(junk1tm): print the name of the struct type as well?
		pass.Reportf(pos, "exported fields should be annotated with the %q tag", fn.Tag)
	}
)

// run starts the analysis.
func run(pass *analysis.Pass, funcs map[string]Func) (any, error) {
	mainModule, err := mainModulePackages()
	if err != nil {
		return nil, err
	}

	// store previous reports to prevent reporting
	// the same struct more than once (if reportOnce is true).
	type report struct {
		pos token.Pos // the position for report.
		tag string    // the missing struct tag.
	}
	reports := make(map[report]struct{})

	walk := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	filter := []ast.Node{(*ast.CallExpr)(nil)}

	walk.Preorder(filter, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return // not a function call.
		}

		caller := typeutil.StaticCallee(pass.TypesInfo, call)
		if caller == nil {
			return // not a static call.
		}

		fn, ok := funcs[caller.FullName()]
		if !ok {
			return // the function is not supported.
		}

		if len(call.Args) <= fn.ArgPos {
			return // TODO(junk1tm): return a proper error.
		}

		arg := call.Args[fn.ArgPos]
		if unary, ok := arg.(*ast.UnaryExpr); ok {
			arg = unary.X // e.g. json.Marshal(&foo)
		}

		initialPos := token.NoPos
		switch arg := arg.(type) {
		case *ast.Ident: // e.g. json.Marshal(foo)
			if arg.Obj == nil {
				return // e.g. json.Marshal(nil)
			}
			initialPos = arg.Obj.Pos()
		case *ast.CompositeLit: // e.g. json.Marshal(struct{}{})
			initialPos = arg.Pos()
		}

		checker := checker{
			mainModule: mainModule,
			seenTypes:  make(map[string]struct{}),
		}

		t := pass.TypesInfo.TypeOf(arg)
		st, ok := checker.parseStructType(t, initialPos)
		if !ok {
			return // not a struct argument.
		}

		result, ok := checker.checkStructType(st, fn.Tag)
		if ok {
			return // nothing to report.
		}

		r := report{result.Pos, fn.Tag}
		if _, ok := reports[r]; ok && reportOnce {
			return // already reported.
		}

		reportf(pass, result.Pos, fn)
		reports[r] = struct{}{}
	})

	return nil, nil
}

// structType is an extension for types.Struct.
// The content of the fields depends on whether the type is named or not.
type structType struct {
	*types.Struct
	Name string    // for types.Named: the type's name; for anonymous: a placeholder string.
	Pos  token.Pos // for types.Named: the type's position; for anonymous: the corresponding identifier's position.
}

// checker parses and checks struct types.
type checker struct {
	mainModule map[string]struct{} // do not check types outside of the main module; see issue #17.
	seenTypes  map[string]struct{} // prevent panic on recursive types; see issue #16.
}

// parseStructType parses the given types.Type, returning the underlying struct type.
func (c *checker) parseStructType(t types.Type, pos token.Pos) (*structType, bool) {
	for {
		// unwrap pointers (if any) first.
		ptr, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = ptr.Elem()
	}

	switch t := t.(type) {
	case *types.Named: // a struct of the named type.
		pkg := t.Obj().Pkg().Path()
		if _, ok := c.mainModule[pkg]; !ok {
			return nil, false
		}
		s, ok := t.Underlying().(*types.Struct)
		if !ok {
			return nil, false
		}
		return &structType{
			Struct: s,
			Pos:    t.Obj().Pos(),
			Name:   t.Obj().Name(),
		}, true

	case *types.Struct: // an anonymous struct.
		return &structType{
			Struct: t,
			Pos:    pos,
			Name:   "{anonymous struct}",
		}, true
	}

	return nil, false
}

// checkStructType recursively checks whether the given struct type is annotated with the tag.
// The result is the type of the first nested struct which fields are not properly annotated.
func (c *checker) checkStructType(st *structType, tag string) (*structType, bool) {
	c.seenTypes[st.String()] = struct{}{}

	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		if !field.Exported() {
			continue
		}

		if _, ok := reflect.StructTag(st.Tag(i)).Lookup(tag); !ok {
			// tag is not required for embedded types; see issue #12.
			if !field.Embedded() {
				return st, false
			}
		}

		nested, ok := c.parseStructType(field.Type(), st.Pos) // TODO(junk1tm): or field.Pos()?
		if !ok {
			continue
		}
		if _, ok := c.seenTypes[nested.String()]; ok {
			continue
		}
		if result, ok := c.checkStructType(nested, tag); !ok {
			return result, false
		}
	}

	return nil, true
}
