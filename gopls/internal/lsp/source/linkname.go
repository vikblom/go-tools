package source

import (
	"context"
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"runtime"
	"strings"

	"golang.org/x/tools/gopls/internal/lsp/protocol"
)

// Compare to source.Identifier
// ParseLinkname could be useful for both "go to definition" and "get references".
// Consider splitting out the finding of the identifier and checking if it already has a body.
// Having a body implies there could be linknamed references.
// Having no body implies there could be linknamed definitions.

// ParseLinkname attempts to parse a go:linkname declaration at the given pos.
// If successful, it returns the package path and object name referenced by the second
// argument of the linkname directive.
//
// If the position is not in a go:linkname directive, or parsing fails, it returns "", "".
//
// TODO: Is it possible to warn on error?
func ParseLinkname(ctx context.Context, snapshot Snapshot, fh FileHandle, pos protocol.Position) (pkgPath, name string) {
	pgf, err := snapshot.ParseGo(ctx, fh, ParseFull)
	if err != nil {
		return "", ""
	}
	if !importsUnsafe(pgf.File) {
		return "", ""
	}

	var local string
	fset := snapshot.FileSet()
	for _, decl := range pgf.File.Decls {
		if fun, ok := decl.(*ast.FuncDecl); ok {
			if fun.Body != nil {
				continue
			}
			fmt.Printf("FUN at: %v %v\n", fun.Name.Pos(), fset.Position(fun.Name.Pos()))

			// Does this decl cover the pos we are looking for?
			at := fset.Position(fun.Name.Pos())
			if at.Line == int(pos.Line+1) {
				if at.Column <= int(pos.Character+1) && int(pos.Character+1) <= fset.Position(fun.Name.End()).Column {
					local = fun.Name.Name
					break
				}
			}
		}
		// TODO: Or GenDecl
	}
	fmt.Printf("IDEN: %q\n", local)
	if local == "" {
		return "", ""
	}

	var linkname string
	for _, cg := range pgf.File.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//go:linkname") {
				args := strings.Split(c.Text, " ")
				if len(args) < 3 {
					continue
				}
				if args[1] == local {
					linkname = args[2]
					break
				}
			}
		}
	}

	// Split the pkg from the identifier.
	dot := strings.LastIndexByte(linkname, '.')
	if dot < 0 {
		return "", ""
	}
	return linkname[:dot], linkname[dot+1:]
}

func importsUnsafe(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path.Value == `"unsafe"` {
			return true
		}
	}
	return false
}

// Deprecated: Didn't find a good way to check if the body is nil.
// Could look up how the compiler parses decls.
func ParseLinknameAlt(ctx context.Context, snapshot Snapshot, fh FileHandle, pos protocol.Position) (pkgPath, name string) {
	src, err := fh.Read()
	if err != nil {
		return "", ""
	}

	// TODO: This only happens if the file imports "unsafe".
	// Looking for //go:linkname local linkname
	// on a decl w/o body.
	var local string
	var linkname string

	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	// First pass, find the decl we are going from.
	s.Init(file, src, nil /* no error handler */, scanner.ScanComments)
	for {
		atPos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		at := fset.Position(atPos)

		// TODO: How is this actually matched?
		if at.Line-1 == int(pos.Line) && at.Column-1 == int(pos.Character) {
			// fmt.Printf("at: %s -- %s\n", tok, lit)
			if tok != token.IDENT {
				return "", ""
			}
			local = lit
			break
		}
	}
	if local == "" {
		return "", ""
	}
	// FIXME: If local does have a body, the linkname points at a reference, not a definition.
	// Jumping there does not make sense for "goto definition".
	// Manually parsing seems like a pain.
	// Was there a function for this?
	// rg -I -t go '^(.*)(Parse[A-z]+)\(.*$' -r '$2' | uniq

	// Second pass, find the first linkname directive for local.
	s.Init(file, src, nil /* no error handler */, scanner.ScanComments)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}

		if tok == token.COMMENT && strings.HasPrefix(lit, "//go:linkname") {
			//fmt.Printf("%s\t%s\t%q\n", snapshot.FileSet().Position(atPos), tok, lit)
			args := strings.Split(lit, " ")
			if len(args) < 3 {
				// Directive 'go:linkname local' is not navigable.
				continue
			}
			if args[1] == local {
				linkname = args[2]
				break
			}
		}
	}

	// Split the pkg from the identifier.
	dot := strings.LastIndexByte(linkname, '.')
	if dot < 0 {
		return "", ""
	}
	return linkname[:dot], linkname[dot+1:]
}

// FindLinkname searches dependencies of packages containing fh for an object
// with linker name matching the given package path and name.
func FindLinkname(ctx context.Context, snapshot Snapshot, fh FileHandle, pos protocol.Position, pkgPath, name string) ([]protocol.Location, error) {
	// TODO: Check what the behaviour of metas is in bigger examples.
	metas, err := snapshot.MetadataForFile(ctx, fh.URI())
	if err != nil {
		return nil, nil
	}
	if len(metas) == 0 {
		return nil, nil
	}

	// Find dep starting from narrowest package metadata.
	id, ok := findPackageInDeps(snapshot, pkgPath, metas[0])
	if !ok {
		return nil, nil
	}
	fmt.Printf("ID: %+v\n", id)

	// When found, type check the desired package (snapshot.TypeCheck in TypecheckFull mode),
	pkgs, err := snapshot.TypeCheck(ctx, TypecheckFull, id)
	if err != nil {
		return nil, nil
	}
	if len(pkgs) != 1 { // one id
		return nil, nil
	}
	pkg := pkgs[0]

	// Get an ast.Node that can be packed into mapping?
	node, ok := findIdentifierInPackage(pkg, name)
	if !ok {
		return nil, nil
	}

	runtime.Breakpoint()
	mapRange, err := posToMappedRange(pkg, node.Pos(), node.End())
	if err != nil {
		return nil, nil
	}
	decRange, err := mapRange.Range()
	if err != nil {
		return nil, nil
	}
	loc := protocol.Location{
		Range: decRange,
		URI:   protocol.DocumentURI(mapRange.URI()),
	}
	return []protocol.Location{loc}, nil
}

func findPackageInDeps(snapshot Snapshot, pkgPath string, meta *Metadata) (PackageID, bool) {
	seen := map[PackageID]struct{}{}
	metas := []*Metadata{meta}
	for len(metas) > 0 {
		meta := metas[0]
		metas = metas[1:]
		seen[meta.ID] = struct{}{}

		if string(meta.ID) == pkgPath {
			return meta.ID, true
		}

		for _, id := range meta.DepsByPkgPath {
			// FIXME: Should we filter on deps that prefix the desired pkg?
			metas = append(metas, snapshot.Metadata(id))
		}
	}
	return "", false
}

func findIdentifierInPackage(pkg Package, name string) (ast.Node, bool) {
	for _, pgf := range pkg.CompiledGoFiles() {
		for _, decl := range pgf.File.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == name {
					return d.Name, true
				}
			}
			// TODO: GenDecl
		}
	}
	return nil, false
}
