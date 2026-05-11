package generator

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const annotationPrefix = "+go-sumtype-accessor="

type Config struct {
	Dir    string
	Output string
}

type structInfo struct {
	name   string
	fields map[string]string
}

type fieldAccessor struct {
	fieldName string
	fieldType string
	getter    string
	setter    string
}

type generationTarget struct {
	interfaceName string
	marker        string
	structs       []structInfo
	accessors     []fieldAccessor
}

func Generate(cfg Config) error {
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Output == "" {
		cfg.Output = "sumtype_accessors.go"
	}

	fset := token.NewFileSet()
	packageName, files, err := parsePackageFiles(fset, cfg.Dir)
	if err != nil {
		return err
	}

	structsByInterface := map[string][]structInfo{}
	collectPackageInfo(fset, files, structsByInterface)
	if len(structsByInterface) == 0 {
		return errors.New("no sumtype accessor annotations found")
	}

	var interfaceNames []string
	for interfaceName := range structsByInterface {
		interfaceNames = append(interfaceNames, interfaceName)
	}
	sort.Strings(interfaceNames)

	var targets []generationTarget
	for _, interfaceName := range interfaceNames {
		structs := structsByInterface[interfaceName]
		accessors, err := commonFieldAccessors(interfaceName, structs)
		if err != nil {
			return err
		}
		targets = append(targets, generationTarget{
			interfaceName: interfaceName,
			marker:        markerMethodName(interfaceName),
			structs:       structs,
			accessors:     accessors,
		})
	}

	content, err := render(packageName, targets)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Dir, cfg.Output), content, 0o644)
}

func parsePackageFiles(fset *token.FileSet, dir string) (string, []*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}

	var packageName string
	var files []*ast.File
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return "", nil, err
		}
		if packageName == "" {
			packageName = file.Name.Name
		}
		if file.Name.Name != packageName {
			return "", nil, fmt.Errorf("expected one package in %s, found %s and %s", dir, packageName, file.Name.Name)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return "", nil, fmt.Errorf("no go files found in %s", dir)
	}
	return packageName, files, nil
}

func collectPackageInfo(fset *token.FileSet, files []*ast.File, structsByInterface map[string][]structInfo) {
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				typ, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				interfaceName := annotationValue(ts.Doc, gen.Doc)
				if interfaceName == "" {
					continue
				}
				structsByInterface[interfaceName] = append(structsByInterface[interfaceName], structInfo{
					name:   ts.Name.Name,
					fields: structFields(fset, typ),
				})
			}
		}
	}
	for interfaceName := range structsByInterface {
		sort.Slice(structsByInterface[interfaceName], func(i, j int) bool {
			return structsByInterface[interfaceName][i].name < structsByInterface[interfaceName][j].name
		})
	}
}

func structFields(fset *token.FileSet, st *ast.StructType) map[string]string {
	fields := map[string]string{}
	for _, field := range st.Fields.List {
		if len(field.Names) != 1 {
			continue
		}
		name := field.Names[0]
		if !name.IsExported() {
			continue
		}
		fields[name.Name] = exprString(fset, field.Type)
	}
	return fields
}

func annotationValue(groups ...*ast.CommentGroup) string {
	for _, group := range groups {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
			if value, ok := strings.CutPrefix(text, annotationPrefix); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func commonFieldAccessors(interfaceName string, structs []structInfo) ([]fieldAccessor, error) {
	if len(structs) == 0 {
		return nil, fmt.Errorf("%s: no annotated structs found", interfaceName)
	}

	common := map[string]string{}
	maps.Copy(common, structs[0].fields)
	for _, st := range structs[1:] {
		for name, typ := range common {
			otherType, ok := st.fields[name]
			if !ok || otherType != typ {
				delete(common, name)
			}
		}
	}

	var names []string
	for name := range common {
		names = append(names, name)
	}
	sort.Strings(names)

	var accessors []fieldAccessor
	for _, fieldName := range names {
		fieldType := common[fieldName]
		accessors = append(accessors, fieldAccessor{
			fieldName: fieldName,
			fieldType: fieldType,
			getter:    "Get" + fieldName,
			setter:    "Set" + fieldName,
		})
	}
	return accessors, nil
}

func render(packageName string, targets []generationTarget) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by go-sumtype-accessor; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", packageName)

	for _, target := range targets {
		fmt.Fprintf(&b, "type %s interface {\n", target.interfaceName)
		fmt.Fprintf(&b, "%s()\n", target.marker)
		for _, accessor := range target.accessors {
			fmt.Fprintf(&b, "%s() %s\n", accessor.getter, accessor.fieldType)
			fmt.Fprintf(&b, "%s(%s)\n", accessor.setter, accessor.fieldType)
		}
		fmt.Fprint(&b, "}\n\n")

		for _, st := range target.structs {
			fmt.Fprintf(&b, "func (*%s) %s() {}\n\n", st.name, target.marker)
			for _, accessor := range target.accessors {
				fmt.Fprintf(&b, "func (v *%s) %s() %s {\n", st.name, accessor.getter, accessor.fieldType)
				fmt.Fprintf(&b, "return v.%s\n", accessor.fieldName)
				fmt.Fprint(&b, "}\n\n")
				fmt.Fprintf(&b, "func (v *%s) %s(value %s) {\n", st.name, accessor.setter, accessor.fieldType)
				fmt.Fprintf(&b, "v.%s = value\n", accessor.fieldName)
				fmt.Fprint(&b, "}\n\n")
			}
		}
	}

	return format.Source(b.Bytes())
}

func exprString(fset *token.FileSet, expr ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, expr)
	return b.String()
}

func markerMethodName(interfaceName string) string {
	return "is" + interfaceName
}
