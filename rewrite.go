// Package generic generates package with type replacements.
package generic

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// rewritePkgName sets current package name.
func rewritePkgName(node *ast.File, pkgName string) {
	node.Name.Name = pkgName
}

// rewriteIdent converts TypeXXX to its replacement defined in typeMap.
func rewriteIdent(node *ast.File, typeMap map[string]Target, fset *token.FileSet) {
	var used []string
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if x.Obj == nil || x.Obj.Kind != ast.Typ {
				return false
			}
			to, ok := typeMap[x.Name]
			if !ok {
				return false
			}
			x.Name = to.Ident

			if to.Import == "" {
				return false
			}
			var found bool
			for _, im := range used {
				if im == to.Import {
					found = true
					break
				}
			}
			if !found {
				used = append(used, to.Import)
			}
			return false
		}
		return true
	})
	for _, im := range used {
		astutil.AddImport(fset, node, im)
	}
}

// removeTypeDecl removes type declarations defined in typeMap.
func removeTypeDecl(node *ast.File, typeMap map[string]Target) {
	for i := len(node.Decls) - 1; i >= 0; i-- {
		genDecl, ok := node.Decls[i].(*ast.GenDecl)
		if !ok {
			continue
		}
		if genDecl.Tok != token.TYPE {
			continue
		}
		var remove bool
		for _, spec := range genDecl.Specs {
			typeSpec := spec.(*ast.TypeSpec)

			_, ok = typeMap[typeSpec.Name.Name]
			if !ok {
				continue
			}

			_, ok = typeSpec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			remove = true
			break
		}
		if remove {
			node.Decls = append(node.Decls[:i], node.Decls[i+1:]...)
		}
	}
}

// findDecl finds type and related declarations.
func findDecl(node *ast.File) (ret []ast.Decl) {
	for _, decl := range node.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if genDecl.Tok != token.TYPE {
			continue
		}

		for _, spec := range genDecl.Specs {
			typeSpec := spec.(*ast.TypeSpec)

			// Replace a complex declaration with a dummy idenifier.
			//
			// It seems simpler to check whether a type is defined.
			typeSpec.Type = &ast.Ident{
				Name: "uint32",
			}
		}

		ret = append(ret, decl)
	}
	return
}

// rewriteTopLevelIdent adds a prefix to top-level identifiers and their uses.
//
// This prevents name conflicts when a package is rewritten to PWD.
func rewriteTopLevelIdent(node *ast.File, prefix string, typeMap map[string]Target) {
	prefixIdent := func(name string) string {
		return lintName(fmt.Sprintf("%s_%s", prefix, name))
	}

	declMap := make(map[interface{}]string)
	for _, decl := range node.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Recv != nil {
				continue
			}
			decl.Name.Name = prefixIdent(decl.Name.Name)
			declMap[decl] = decl.Name.Name
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					obj := spec.Name.Obj
					if obj != nil && obj.Kind == ast.Typ {
						if to, ok := typeMap[obj.Name]; ok && spec.Name.Name == to.Ident {
							// If this identifier is already rewritten before, we don't need to prefix it.
							continue
						}
					}
					spec.Name.Name = prefixIdent(spec.Name.Name)
					declMap[spec] = spec.Name.Name
				case *ast.ValueSpec:
					for _, ident := range spec.Names {
						ident.Name = prefixIdent(ident.Name)
						declMap[spec] = ident.Name
					}
				}
			}
		}
	}

	// After top-level identifiers are renamed, find where they are used, and rewrite those.
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if x.Obj == nil || x.Obj.Decl == nil {
				return false
			}
			name, ok := declMap[x.Obj.Decl]
			if !ok {
				return false
			}
			x.Name = name
			return false
		}
		return true
	})
}

// walkSource visits all .go files in a package path except tests.
func walkSource(pkgPath string, sourceFunc func(string) error) error {
	fi, err := ioutil.ReadDir(pkgPath)
	if err != nil {
		return err
	}
	for _, info := range fi {
		if info.IsDir() {
			continue
		}
		path := fmt.Sprintf("%s/%s", pkgPath, info.Name())
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		err = sourceFunc(path)
		if err != nil {
			return err
		}
	}
	return nil
}

type packageTarget struct {
	SameDir bool
	NewName string
	NewPath string
}

// parsePackageTarget finds where a package can be rewritten.
func parsePackageTarget(path string) (*packageTarget, error) {
	t := new(packageTarget)
	if strings.HasPrefix(path, ".") {
		t.SameDir = true
		t.NewPath = strings.TrimPrefix(path, ".")
		t.NewName = os.Getenv("GOPACKAGE")
		if t.NewName == "" {
			return nil, errors.New("GOPACKAGE cannot be empty")
		}
	} else {
		t.NewPath = path
		t.NewName = filepath.Base(path)
	}

	return t, nil
}

// RewritePackage applies type replacements on a package in GOPATH, and saves results as a new package in $PWD.
//
// If there is a dir with the same name as newPkgPath, it will first be removed. It is possible to re-run this
// to update a generic package.
func RewritePackage(pkgPath string, newPkgPath string, typeMap map[string]Target) error {
	var err error

	pt, err := parsePackageTarget(newPkgPath)
	if err != nil {
		return err
	}

	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	err = walkSource(fmt.Sprintf("%s/src/%s", os.Getenv("GOPATH"), pkgPath), func(path string) error {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files[path] = f
		return nil
	})
	if err != nil {
		return err
	}

	// Gather ast.File to create ast.Package.
	// ast.NewPackage will try to resolve unresolved identifiers.
	ast.NewPackage(fset, files, nil, nil)

	// Apply AST changes and refresh.
	buf := new(bytes.Buffer)
	var tc []*ast.File
	for path, f := range files {
		rewritePkgName(f, pt.NewName)
		removeTypeDecl(f, typeMap)
		rewriteIdent(f, typeMap, fset)
		if pt.SameDir {
			rewriteTopLevelIdent(f, pt.NewPath, typeMap)
		}

		// AST in dirty state; refresh
		buf.Reset()
		err = printer.Fprint(buf, fset, f)
		if err != nil {
			return err
		}
		f, err = parser.ParseFile(fset, "", buf, 0)
		if err != nil {
			printer.Fprint(os.Stderr, fset, f)
			return err
		}
		files[path] = f
		tc = append(tc, f)
	}

	// Type-check.
	if pt.SameDir {
		// Also include same-dir files.
		// However, it is silly to add the entire file,
		// because that file might have identifiers from another generic package.
		err = walkSource(".", func(path string) error {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			decl := findDecl(f)
			if len(decl) > 0 {
				tc = append(tc, &ast.File{
					Decls: decl,
					Name:  f.Name,
				})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	conf := types.Config{Importer: importer.Default()}
	_, err = conf.Check("", fset, tc, nil)
	if err != nil {
		for _, f := range tc {
			printer.Fprint(os.Stderr, fset, f)
		}
		return err
	}

	if pt.SameDir {
		for path, f := range files {
			// Print ast to file.
			var dest *os.File
			dest, err = os.Create(fmt.Sprintf("%s_%s", pt.NewPath, filepath.Base(path)))
			if err != nil {
				return err
			}
			defer dest.Close()

			err = format.Node(dest, fset, f)
			if err != nil {
				return err
			}
		}
		return nil
	}

	err = os.RemoveAll(pt.NewPath)
	if err != nil {
		return err
	}

	err = os.MkdirAll(pt.NewPath, 0777)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(pt.NewPath)
		}
	}()

	for path, f := range files {
		// Print ast to file.
		var dest *os.File
		dest, err = os.Create(fmt.Sprintf("%s/%s", pt.NewPath, filepath.Base(path)))
		if err != nil {
			return err
		}
		defer dest.Close()

		err = format.Node(dest, fset, f)
		if err != nil {
			return err
		}
	}
	return nil
}
