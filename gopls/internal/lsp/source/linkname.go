package source

import (
	"context"
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"strings"

	"golang.org/x/tools/gopls/internal/lsp/protocol"
)

// Compare to source.Identifier

// ParseLinkname attempts to parse a go:linkname declaration at the given pos.
// If successful, it returns the package path and object name referenced by the second
// argument of the linkname directive.
//
// If the position is not in a go:linkname directive, or parsing fails, it returns "", "".
//
// TODO: Is it possible to warn on error?
func ParseLinkname(ctx context.Context, snapshot Snapshot, fh FileHandle, pos protocol.Position) (pkgPath, name string) {
	src, err := fh.Read()
	if err != nil {
		return "", ""
	}

	// TODO: Not sure how to get the identifier we are looking at w/o this.
	// Is it possible to read directly at the protocol.Position somehow?
	// Also need it for a *token.File to initiate scanner.
	// pgf, err := snapshot.ParseGo(ctx, fh, ParseFull)
	// if err != nil {
	// 	return "", ""
	// }
	// lookingFor, err := pgf.Mapper.Pos(pos)
	// if err != nil {
	// 	return "", ""
	// }
	// var iden string
	// fset := snapshot.FileSet()
	// for _, decl := range pgf.File.Decls {
	// 	if fun, ok := decl.(*ast.FuncDecl); ok {
	// 		at := fset.Position(fun.Pos())
	// 		fmt.Printf("at: %+v %+v\n", at, fun)
	// 		if at.Line-1 == int(pos.Line) && at.Column-1 == int(pos.Character) {
	// 			iden = fun.Name.Name
	// 			break
	// 		}
	// 	}
	// }
	// if iden == "" {
	// 	return "", ""
	// }
	// fmt.Printf("IDEN: %+v\n", iden)

	// pgf.Tok
	// pgf.Mapper.TokFile

	// Looking for //go:linkname local linkname
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
			//fmt.Printf("at: %s -- %s\n", tok, lit)
			if tok.String() != "IDENT" {
				return "", ""
			}
			local = lit
		}
	}
	if local == "" {
		return "", ""
	}

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

	metas, err := snapshot.MetadataForFile(ctx, fh.URI())
	if err != nil {
		return nil, nil
	}
	if len(metas) == 0 {
		return nil, nil
	}
	var pkgID PackageID
	// Start from narrowest package.
	seen := map[PackageID]struct{}{}
	metas = metas[:1]
	for len(metas) > 0 && pkgID == "" {
		meta := metas[0]
		metas = metas[1:]
		seen[meta.ID] = struct{}{}

		for _, id := range meta.DepsByPkgPath {
			if !strings.HasPrefix(pkgPath, string(id)) {
				continue
			}
			if pkgPath == string(id) {
				pkgID = id
				break
			}
			// FIXME: Probably does not recurse correctly.
			metas = append(metas, snapshot.Metadata(id))
		}
	}
	fmt.Printf("ID: %+v\n", pkgID)

	_ = pkgID
	// TODO

	// When found, type check the desired package (snapshot.TypeCheck in TypecheckFull mode),

	pkgs, err := snapshot.TypeCheck(ctx, TypecheckFull, pkgID)
	if err != nil {
		return nil, nil
	}
	for _, pkg := range pkgs {
		fmt.Printf("PKG: %+v\n", pkg)
		for _, pgf := range pkg.CompiledGoFiles() {
			for _, decl := range pgf.File.Decls {
				switch decl := decl.(type) {
				case *ast.FuncDecl:
					if decl.Name.Name == name {
						mapRange, err := posToMappedRange(pkg, decl.Pos(), decl.End())
						if err != nil {
							return nil, nil
						}
						pRange, err := mapRange.Range()
						if err != nil {
							return nil, nil
						}
						loc := protocol.Location{
							Range: pRange,
							URI:   protocol.DocumentURI(mapRange.URI()),
						}
						return []protocol.Location{loc}, nil
					}
				}
			}
		}
	}

	return nil, nil
}
