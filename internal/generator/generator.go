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
	"unicode"
)

const annotationPrefix = "+go-sumtype-accessor="

type Config struct {
	Dir    string
	Suffix string
}

type methodSpec struct {
	name    string
	params  []string
	results []string
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
	packageName   string
	marker        string
	structs       []structInfo
	accessors     []fieldAccessor
}

func Generate(cfg Config) error {
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Suffix == "" {
		cfg.Suffix = "_sumtype.go"
	}

	fset := token.NewFileSet()
	packageName, files, err := parsePackageFiles(fset, cfg.Dir)
	if err != nil {
		return err
	}

	methodsByInterface := map[string]map[string]methodSpec{}
	structsByInterface := map[string][]structInfo{}
	if err := collectPackageInfo(fset, files, methodsByInterface, structsByInterface); err != nil {
		return err
	}
	if len(structsByInterface) == 0 {
		return errors.New("no sumtype accessor annotations found")
	}

	for interfaceName, structs := range structsByInterface {
		methods, ok := methodsByInterface[interfaceName]
		if !ok {
			return fmt.Errorf("interface %s not found", interfaceName)
		}
		marker, err := markerMethod(methods)
		if err != nil {
			return fmt.Errorf("%s: %w", interfaceName, err)
		}
		accessors, err := commonFieldAccessors(interfaceName, structs, methods)
		if err != nil {
			return err
		}
		content, err := render(generationTarget{
			interfaceName: interfaceName,
			packageName:   packageName,
			marker:        marker,
			structs:       structs,
			accessors:     accessors,
		})
		if err != nil {
			return err
		}

		filename := strings.ToLower(interfaceName) + cfg.Suffix
		if err := os.WriteFile(filepath.Join(cfg.Dir, filename), content, 0o644); err != nil {
			return err
		}
	}
	return nil
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

func collectPackageInfo(fset *token.FileSet, files []*ast.File, methodsByInterface map[string]map[string]methodSpec, structsByInterface map[string][]structInfo) error {
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
				switch typ := ts.Type.(type) {
				case *ast.InterfaceType:
					methodsByInterface[ts.Name.Name] = methodsFromInterface(typ)
				case *ast.StructType:
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
	}
	for interfaceName := range structsByInterface {
		sort.Slice(structsByInterface[interfaceName], func(i, j int) bool {
			return structsByInterface[interfaceName][i].name < structsByInterface[interfaceName][j].name
		})
	}
	return nil
}

func methodsFromInterface(iface *ast.InterfaceType) map[string]methodSpec {
	methods := map[string]methodSpec{}
	for _, field := range iface.Methods.List {
		if len(field.Names) != 1 {
			continue
		}
		fn, ok := field.Type.(*ast.FuncType)
		if !ok {
			continue
		}
		name := field.Names[0].Name
		methods[name] = methodSpec{
			name:    name,
			params:  fieldTypes(fn.Params),
			results: fieldTypes(fn.Results),
		}
	}
	return methods
}

func structFields(fset *token.FileSet, st *ast.StructType) map[string]string {
	fields := map[string]string{}
	for _, field := range st.Fields.List {
		if len(field.Names) != 1 {
			continue
		}
		fields[field.Names[0].Name] = exprString(fset, field.Type)
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

func markerMethod(methods map[string]methodSpec) (string, error) {
	var markers []string
	for name, method := range methods {
		if isUnexported(name) && len(method.params) == 0 && len(method.results) == 0 {
			markers = append(markers, name)
		}
	}
	if len(markers) != 1 {
		return "", fmt.Errorf("expected exactly one unexported marker method, found %d", len(markers))
	}
	return markers[0], nil
}

func commonFieldAccessors(interfaceName string, structs []structInfo, methods map[string]methodSpec) ([]fieldAccessor, error) {
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
		accessor := fieldAccessor{fieldName: fieldName, fieldType: fieldType}

		getter := "Get" + fieldName
		if _, ok := methods[getter]; ok {
			if err := validateGetter(methods, getter, fieldType); err != nil {
				return nil, err
			}
			accessor.getter = getter
		}

		setter := "Set" + fieldName
		if _, ok := methods[setter]; ok {
			if err := validateSetter(methods, setter, fieldType); err != nil {
				return nil, err
			}
			accessor.setter = setter
		}

		if accessor.getter != "" || accessor.setter != "" {
			accessors = append(accessors, accessor)
		}
	}
	return accessors, nil
}

func validateGetter(methods map[string]methodSpec, name string, fieldType string) error {
	method := methods[name]
	if len(method.params) != 0 || len(method.results) != 1 {
		return fmt.Errorf("getter method %s must have signature func() %s", name, fieldType)
	}
	if method.results[0] != fieldType {
		return fmt.Errorf("getter method %s returns %s, field type is %s", name, method.results[0], fieldType)
	}
	return nil
}

func validateSetter(methods map[string]methodSpec, name string, fieldType string) error {
	method := methods[name]
	if len(method.params) != 1 || len(method.results) != 0 {
		return fmt.Errorf("setter method %s must have signature func(%s)", name, fieldType)
	}
	if method.params[0] != fieldType {
		return fmt.Errorf("setter method %s accepts %s, field type is %s", name, method.params[0], fieldType)
	}
	return nil
}

func render(target generationTarget) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by go-sumtype-accessor; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", target.packageName)

	for _, st := range target.structs {
		fmt.Fprintf(&b, "func (*%s) %s() {}\n\n", st.name, target.marker)
		for _, accessor := range target.accessors {
			if accessor.getter != "" {
				fmt.Fprintf(&b, "func (v *%s) %s() %s {\n", st.name, accessor.getter, accessor.fieldType)
				fmt.Fprintf(&b, "return v.%s\n", accessor.fieldName)
				fmt.Fprint(&b, "}\n\n")
			}
			if accessor.setter != "" {
				fmt.Fprintf(&b, "func (v *%s) %s(value %s) {\n", st.name, accessor.setter, accessor.fieldType)
				fmt.Fprintf(&b, "v.%s = value\n", accessor.fieldName)
				fmt.Fprint(&b, "}\n\n")
			}
		}
	}

	return format.Source(b.Bytes())
}

func fieldTypes(fields *ast.FieldList) []string {
	if fields == nil {
		return nil
	}
	var types []string
	fset := token.NewFileSet()
	for _, field := range fields.List {
		typeName := exprString(fset, field.Type)
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			types = append(types, typeName)
		}
	}
	return types
}

func exprString(fset *token.FileSet, expr ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, expr)
	return b.String()
}

func isUnexported(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)[0]
	return !unicode.IsUpper(r)
}
