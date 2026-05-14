package generator

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/dave/jennifer/jen"
	"golang.org/x/tools/go/packages"
)

const (
	annotationPrefix          = "+go-sumtype-accessor="
	typeParamAnnotationPrefix = "+go-sumtype-accessor:generic-facets="
	ignoreTagKey              = "sumtype"
	DefaultOutput             = "sumtype_accessors.go"
)

type Config struct {
	Dir    string
	Output string
}

type structInfo struct {
	name       string
	typeParams []typeParamInfo
	fields     map[string]fieldInfo
}

type typeParamInfo struct {
	name       string
	constraint types.Type
}

type fieldInfo struct {
	typ types.Type
}

type fieldAccessor struct {
	fieldName string
	fieldType types.Type
	getter    string
	setter    string
}

type generationTarget struct {
	interfaceName string
	marker        string
	structs       []structInfo
	accessors     []fieldAccessor
}

type typeParamTarget struct {
	interfaceName string
	marker        string
	st            structInfo
	accessors     []fieldAccessor
	facets        []typeParamFacet
	combinations  []typeParamFacetCombination
}

type typeParamFacet struct {
	interfaceName string
	typeParam     typeParamInfo
	method        string
	accessors     []fieldAccessor
}

type typeParamFacetCombination struct {
	interfaceName string
	facets        []typeParamFacet
}

type interfaceStub struct {
	name       string
	typeParams []string
}

func Generate(cfg Config) error {
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Output == "" {
		cfg.Output = DefaultOutput
	}

	pkg, err := loadPackage(cfg.Dir, cfg.Output)
	if err != nil {
		return err
	}

	structsByInterface := map[string][]structInfo{}
	typeParamStructsByInterface := map[string][]structInfo{}
	collectPackageInfo(pkg, structsByInterface, typeParamStructsByInterface)
	if len(structsByInterface) == 0 && len(typeParamStructsByInterface) == 0 {
		return errors.New("no sumtype accessor annotations found")
	}

	interfaceNames := slices.Sorted(maps.Keys(structsByInterface))
	targets := make([]generationTarget, 0, len(interfaceNames))
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

	typeParamInterfaceNames := slices.Sorted(maps.Keys(typeParamStructsByInterface))
	typeParamTargets := make([]typeParamTarget, 0, len(typeParamInterfaceNames))
	for _, interfaceName := range typeParamInterfaceNames {
		structs := typeParamStructsByInterface[interfaceName]
		target, err := newTypeParamTarget(interfaceName, structs)
		if err != nil {
			return err
		}
		typeParamTargets = append(typeParamTargets, target)
	}

	content, err := render(pkg.Name, pkg.PkgPath, targets, typeParamTargets)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Dir, cfg.Output), content, 0o644)
}

func loadPackage(dir, output string) (*packages.Package, error) {
	overlay, err := outputOverlay(dir, output)
	if err != nil {
		return nil, err
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedImports | packages.NeedDeps,
		Dir:     dir,
		Tests:   false,
		Overlay: overlay,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no go files found in %s", dir)
	}
	var errs []error
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			errs = append(errs, err)
		}
	})
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return pkgs[0], nil
}

func outputOverlay(dir, output string) (map[string][]byte, error) {
	if output == "" {
		return nil, nil
	}
	outputPath, err := outputFilePath(dir, output)
	if err != nil {
		return nil, err
	}
	packageName, stubs, err := packageInterfaceStubs(dir, outputPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			if len(stubs) == 0 {
				return nil, nil
			}
			return map[string][]byte{outputPath: renderInterfaceStubs(packageName, stubs)}, nil
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("output path is a directory: %s", outputPath)
	}
	outputPackage, outputStubs, err := generatedInterfaceStubs(outputPath)
	if err != nil {
		return nil, err
	}
	if packageName == "" {
		packageName = outputPackage
	}
	maps.Copy(stubs, outputStubs)
	return map[string][]byte{
		outputPath: renderInterfaceStubs(packageName, stubs),
	}, nil
}

func outputFilePath(dir, output string) (string, error) {
	outputPath := output
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(dir, outputPath)
	}
	return filepath.Abs(outputPath)
}

func packageInterfaceStubs(dir, outputPath string) (string, map[string]interfaceStub, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, err
	}
	stubs := map[string]interfaceStub{}
	definedTypes := map[string]bool{}
	packageName := ""
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path, err := filepath.Abs(filepath.Join(dir, entry.Name()))
		if err != nil {
			return "", nil, err
		}
		if path == outputPath {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return "", nil, err
		}
		if packageName == "" {
			packageName = file.Name.Name
		}
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
				definedTypes[ts.Name.Name] = true
				if _, ok := ts.Type.(*ast.StructType); !ok {
					continue
				}
				if interfaceName := annotationValue(annotationPrefix, ts.Doc, gen.Doc); interfaceName != "" {
					if _, ok := stubs[interfaceName]; !ok {
						stubs[interfaceName] = interfaceStub{name: interfaceName, typeParams: typeParamNames(ts.TypeParams)}
					}
				}
				if interfaceName := annotationValue(typeParamAnnotationPrefix, ts.Doc, gen.Doc); interfaceName != "" {
					if _, ok := stubs[interfaceName]; !ok {
						stubs[interfaceName] = interfaceStub{name: interfaceName}
					}
					for _, stub := range typeParamFacetStubs(interfaceName, ts.TypeParams) {
						if _, ok := stubs[stub.name]; !ok {
							stubs[stub.name] = stub
						}
					}
				}
			}
		}
	}
	for name := range definedTypes {
		delete(stubs, name)
	}
	return packageName, stubs, nil
}

func generatedInterfaceStubs(path string) (string, map[string]interfaceStub, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return "", nil, err
	}

	stubs := map[string]interfaceStub{}
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
			if _, ok := ts.Type.(*ast.InterfaceType); !ok {
				continue
			}
			stubs[ts.Name.Name] = interfaceStub{name: ts.Name.Name, typeParams: typeParamNames(ts.TypeParams)}
		}
	}
	return file.Name.Name, stubs, nil
}

func renderInterfaceStubs(packageName string, stubs map[string]interfaceStub) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "package %s\n\n", packageName)
	for _, name := range slices.Sorted(maps.Keys(stubs)) {
		stub := stubs[name]
		fmt.Fprintf(&b, "type %s", stub.name)
		writeAnyTypeParams(&b, stub.typeParams)
		b.WriteString(" interface{}\n\n")
	}
	return b.Bytes()
}

func typeParamNames(params *ast.FieldList) []string {
	if params == nil || params.NumFields() == 0 {
		return nil
	}
	names := make([]string, 0, params.NumFields())
	for _, field := range params.List {
		for _, name := range field.Names {
			var b bytes.Buffer
			_ = printer.Fprint(&b, token.NewFileSet(), name)
			names = append(names, b.String())
		}
	}
	return names
}

func typeParamFacetStubs(interfaceName string, params *ast.FieldList) []interfaceStub {
	if params == nil || params.NumFields() == 0 {
		return nil
	}
	var names []string
	for _, field := range params.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	stubs := make([]interfaceStub, 0, len(names))
	for _, name := range names {
		stubs = append(stubs, interfaceStub{
			name:       interfaceName + "With" + name,
			typeParams: []string{name},
		})
	}
	for _, combination := range typeParamNameCombinations(names) {
		stubs = append(stubs, interfaceStub{
			name:       typeParamCombinationInterfaceName(interfaceName, combination),
			typeParams: combination,
		})
	}
	return stubs
}

func typeParamNameCombinations(names []string) [][]string {
	var combinations [][]string
	for mask := 1; mask < 1<<len(names); mask++ {
		if selectedBitCount(mask) < 2 {
			continue
		}
		combination := make([]string, 0, len(names))
		for i, name := range names {
			if mask&(1<<i) != 0 {
				combination = append(combination, name)
			}
		}
		combinations = append(combinations, combination)
	}
	return combinations
}

func selectedBitCount(mask int) int {
	count := 0
	for mask > 0 {
		count += mask & 1
		mask >>= 1
	}
	return count
}

func typeParamCombinationInterfaceName(interfaceName string, names []string) string {
	return interfaceName + "With" + strings.Join(names, "And")
}

func typeParamFacetCombinations(interfaceName string, facets []typeParamFacet) []typeParamFacetCombination {
	var combinations []typeParamFacetCombination
	for mask := 1; mask < 1<<len(facets); mask++ {
		if selectedBitCount(mask) < 2 {
			continue
		}
		combination := make([]typeParamFacet, 0, len(facets))
		names := make([]string, 0, len(facets))
		for i, facet := range facets {
			if mask&(1<<i) != 0 {
				combination = append(combination, facet)
				names = append(names, facet.typeParam.name)
			}
		}
		combinations = append(combinations, typeParamFacetCombination{
			interfaceName: typeParamCombinationInterfaceName(interfaceName, names),
			facets:        combination,
		})
	}
	return combinations
}

func writeAnyTypeParams(b *bytes.Buffer, params []string) {
	if len(params) == 0 {
		return
	}
	b.WriteByte('[')
	for i, name := range params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		b.WriteString(" any")
	}
	b.WriteByte(']')
}

func collectPackageInfo(pkg *packages.Package, structsByInterface, typeParamStructsByInterface map[string][]structInfo) {
	for _, file := range pkg.Syntax {
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
				if _, ok := ts.Type.(*ast.StructType); !ok {
					continue
				}
				interfaceName := annotationValue(annotationPrefix, ts.Doc, gen.Doc)
				typeParamInterfaceName := annotationValue(typeParamAnnotationPrefix, ts.Doc, gen.Doc)
				if interfaceName == "" && typeParamInterfaceName == "" {
					continue
				}
				obj := pkg.TypesInfo.Defs[ts.Name]
				if obj == nil {
					continue
				}
				named, ok := obj.Type().(*types.Named)
				if !ok {
					continue
				}
				stType, ok := named.Underlying().(*types.Struct)
				if !ok {
					continue
				}
				st := structInfo{
					name:       ts.Name.Name,
					typeParams: typeParams(named),
					fields:     structFields(stType),
				}
				if interfaceName != "" {
					structsByInterface[interfaceName] = append(structsByInterface[interfaceName], st)
				}
				if typeParamInterfaceName != "" {
					typeParamStructsByInterface[typeParamInterfaceName] = append(typeParamStructsByInterface[typeParamInterfaceName], st)
				}
			}
		}
	}
	for interfaceName := range structsByInterface {
		slices.SortFunc(structsByInterface[interfaceName], func(a, b structInfo) int {
			return cmp.Compare(a.name, b.name)
		})
	}
	for interfaceName := range typeParamStructsByInterface {
		slices.SortFunc(typeParamStructsByInterface[interfaceName], func(a, b structInfo) int {
			return cmp.Compare(a.name, b.name)
		})
	}
}

func typeParams(named *types.Named) []typeParamInfo {
	params := named.TypeParams()
	if params == nil || params.Len() == 0 {
		return nil
	}
	infos := make([]typeParamInfo, 0, params.Len())
	for param := range params.TypeParams() {
		infos = append(infos, typeParamInfo{
			name:       param.Obj().Name(),
			constraint: param.Constraint(),
		})
	}
	return infos
}

func structFields(st *types.Struct) map[string]fieldInfo {
	fields := make(map[string]fieldInfo, st.NumFields())
	for i := range st.NumFields() {
		field := st.Field(i)
		if !field.Exported() {
			continue
		}
		if reflect.StructTag(st.Tag(i)).Get(ignoreTagKey) == "-" {
			continue
		}
		fields[field.Name()] = fieldInfo{typ: field.Type()}
	}
	return fields
}

func annotationValue(prefix string, groups ...*ast.CommentGroup) string {
	for _, group := range groups {
		if group == nil {
			continue
		}
		for line := range strings.SplitSeq(group.Text(), "\n") {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, prefix); ok {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func newTypeParamTarget(interfaceName string, structs []structInfo) (typeParamTarget, error) {
	if len(structs) == 0 {
		return typeParamTarget{}, fmt.Errorf("%s: no annotated structs found", interfaceName)
	}
	if len(structs) > 1 {
		return typeParamTarget{}, fmt.Errorf("%s: type parameter accessor annotation must be used on exactly one struct", interfaceName)
	}
	st := structs[0]
	facets := make([]typeParamFacet, 0, len(st.typeParams))
	for _, param := range st.typeParams {
		facets = append(facets, typeParamFacet{
			interfaceName: interfaceName + "With" + param.name,
			typeParam:     param,
			method:        "is" + param.name,
			accessors:     typeParamFieldAccessors(st, param.name),
		})
	}
	return typeParamTarget{
		interfaceName: interfaceName,
		marker:        markerMethodName(interfaceName),
		st:            st,
		accessors:     nonTypeParamFieldAccessors(st),
		facets:        facets,
		combinations:  typeParamFacetCombinations(interfaceName, facets),
	}, nil
}

func nonTypeParamFieldAccessors(st structInfo) []fieldAccessor {
	return fieldAccessorsByTypeParamUse(st, func(used map[string]bool) bool {
		return len(used) == 0
	})
}

func typeParamFieldAccessors(st structInfo, paramName string) []fieldAccessor {
	return fieldAccessorsByTypeParamUse(st, func(used map[string]bool) bool {
		return len(used) == 1 && used[paramName]
	})
}

func fieldAccessorsByTypeParamUse(st structInfo, include func(map[string]bool) bool) []fieldAccessor {
	names := slices.Sorted(maps.Keys(st.fields))
	accessors := make([]fieldAccessor, 0, len(names))
	for _, fieldName := range names {
		fieldType := st.fields[fieldName].typ
		used := map[string]bool{}
		collectTypeParamNames(fieldType, used, map[types.Type]bool{})
		if !include(used) {
			continue
		}
		accessors = append(accessors, fieldAccessor{
			fieldName: fieldName,
			fieldType: fieldType,
			getter:    "Get" + fieldName,
			setter:    "Set" + fieldName,
		})
	}
	return accessors
}

func commonFieldAccessors(interfaceName string, structs []structInfo) ([]fieldAccessor, error) {
	if len(structs) == 0 {
		return nil, fmt.Errorf("%s: no annotated structs found", interfaceName)
	}
	for _, st := range structs[1:] {
		if !sameTypeParams(structs[0].typeParams, st.typeParams) {
			return nil, fmt.Errorf("%s: annotated structs must use matching type parameters: %s and %s differ", interfaceName, structs[0].name, st.name)
		}
	}

	common := maps.Clone(structs[0].fields)
	for _, st := range structs[1:] {
		for name, field := range common {
			otherField, ok := st.fields[name]
			if !ok || !sameFieldType(field.typ, otherField.typ) {
				delete(common, name)
			}
		}
	}

	names := slices.Sorted(maps.Keys(common))
	accessors := make([]fieldAccessor, 0, len(names))
	for _, fieldName := range names {
		fieldType := common[fieldName].typ
		accessors = append(accessors, fieldAccessor{
			fieldName: fieldName,
			fieldType: fieldType,
			getter:    "Get" + fieldName,
			setter:    "Set" + fieldName,
		})
	}
	return accessors, nil
}

func sameTypeParams(a, b []typeParamInfo) bool {
	return slices.EqualFunc(a, b, func(a, b typeParamInfo) bool {
		return a.name == b.name && sameFieldType(a.constraint, b.constraint)
	})
}

func sameFieldType(a, b types.Type) bool {
	return sameFieldTypeSeen(a, b, map[typePair]bool{})
}

type typePair struct {
	a types.Type
	b types.Type
}

//nolint:gocyclo // This intentionally mirrors the go/types Type implementations.
func sameFieldTypeSeen(a, b types.Type, seen map[typePair]bool) bool {
	if types.Identical(a, b) {
		return true
	}
	pair := typePair{a: a, b: b}
	if seen[pair] {
		return true
	}
	seen[pair] = true

	switch a := a.(type) {
	case *types.Alias:
		b, ok := b.(*types.Alias)
		return ok && sameTypeName(a.Obj(), b.Obj()) && sameTypeList(a.TypeArgs(), b.TypeArgs(), seen)
	case *types.Array:
		b, ok := b.(*types.Array)
		return ok && a.Len() == b.Len() && sameFieldTypeSeen(a.Elem(), b.Elem(), seen)
	case *types.Basic:
		b, ok := b.(*types.Basic)
		return ok && a.Kind() == b.Kind()
	case *types.Chan:
		b, ok := b.(*types.Chan)
		return ok && a.Dir() == b.Dir() && sameFieldTypeSeen(a.Elem(), b.Elem(), seen)
	case *types.Map:
		b, ok := b.(*types.Map)
		return ok && sameFieldTypeSeen(a.Key(), b.Key(), seen) && sameFieldTypeSeen(a.Elem(), b.Elem(), seen)
	case *types.Interface:
		b, ok := b.(*types.Interface)
		return ok && sameInterfaceType(a, b, seen)
	case *types.Named:
		b, ok := b.(*types.Named)
		return ok && sameTypeName(a.Obj(), b.Obj()) && sameTypeList(a.TypeArgs(), b.TypeArgs(), seen)
	case *types.Pointer:
		b, ok := b.(*types.Pointer)
		return ok && sameFieldTypeSeen(a.Elem(), b.Elem(), seen)
	case *types.Signature:
		b, ok := b.(*types.Signature)
		return ok && sameSignatureType(a, b, seen)
	case *types.Slice:
		b, ok := b.(*types.Slice)
		return ok && sameFieldTypeSeen(a.Elem(), b.Elem(), seen)
	case *types.Struct:
		b, ok := b.(*types.Struct)
		return ok && sameStructType(a, b, seen)
	case *types.TypeParam:
		b, ok := b.(*types.TypeParam)
		return ok && a.Obj().Name() == b.Obj().Name()
	case *types.Union:
		b, ok := b.(*types.Union)
		return ok && sameUnionType(a, b, seen)
	default:
		return false
	}
}

func sameTypeName(a, b *types.TypeName) bool {
	if a.Name() != b.Name() {
		return false
	}
	aPkg := a.Pkg()
	bPkg := b.Pkg()
	if aPkg == nil || bPkg == nil {
		return aPkg == bPkg
	}
	return aPkg.Path() == bPkg.Path()
}

func sameTypeList(a, b *types.TypeList, seen map[typePair]bool) bool {
	aLen := 0
	if a != nil {
		aLen = a.Len()
	}
	bLen := 0
	if b != nil {
		bLen = b.Len()
	}
	if aLen != bLen {
		return false
	}
	for i := range aLen {
		if !sameFieldTypeSeen(a.At(i), b.At(i), seen) {
			return false
		}
	}
	return true
}

func sameInterfaceType(a, b *types.Interface, seen map[typePair]bool) bool {
	if a.NumEmbeddeds() != b.NumEmbeddeds() || a.NumExplicitMethods() != b.NumExplicitMethods() {
		return false
	}
	for i := range a.NumEmbeddeds() {
		if !sameFieldTypeSeen(a.EmbeddedType(i), b.EmbeddedType(i), seen) {
			return false
		}
	}
	for i := range a.NumExplicitMethods() {
		aMethod := a.ExplicitMethod(i)
		bMethod := b.ExplicitMethod(i)
		if aMethod.Name() != bMethod.Name() || !sameSignatureType(aMethod.Type().(*types.Signature), bMethod.Type().(*types.Signature), seen) {
			return false
		}
	}
	return true
}

func sameSignatureType(a, b *types.Signature, seen map[typePair]bool) bool {
	return a.Variadic() == b.Variadic() &&
		sameTupleType(a.Params(), b.Params(), seen) &&
		sameTupleType(a.Results(), b.Results(), seen)
}

func sameTupleType(a, b *types.Tuple, seen map[typePair]bool) bool {
	aLen := 0
	if a != nil {
		aLen = a.Len()
	}
	bLen := 0
	if b != nil {
		bLen = b.Len()
	}
	if aLen != bLen {
		return false
	}
	for i := range aLen {
		if !sameFieldTypeSeen(a.At(i).Type(), b.At(i).Type(), seen) {
			return false
		}
	}
	return true
}

func sameStructType(a, b *types.Struct, seen map[typePair]bool) bool {
	if a.NumFields() != b.NumFields() {
		return false
	}
	for i := range a.NumFields() {
		aField := a.Field(i)
		bField := b.Field(i)
		if aField.Name() != bField.Name() ||
			aField.Embedded() != bField.Embedded() ||
			a.Tag(i) != b.Tag(i) ||
			!sameFieldTypeSeen(aField.Type(), bField.Type(), seen) {
			return false
		}
	}
	return true
}

func sameUnionType(a, b *types.Union, seen map[typePair]bool) bool {
	if a.Len() != b.Len() {
		return false
	}
	for i := range a.Len() {
		aTerm := a.Term(i)
		bTerm := b.Term(i)
		if aTerm.Tilde() != bTerm.Tilde() || !sameFieldTypeSeen(aTerm.Type(), bTerm.Type(), seen) {
			return false
		}
	}
	return true
}

func render(packageName, packagePath string, targets []generationTarget, typeParamTargets []typeParamTarget) ([]byte, error) {
	f := jen.NewFilePathName(packagePath, packageName)
	f.HeaderComment("Code generated by go-sumtype-accessor. DO NOT EDIT.")

	typeRenderer := typeRenderer{}
	for _, target := range targets {
		interfaceType := f.Type().Id(target.interfaceName)
		if typeParamDecls := typeRenderer.typeParamDecls(interfaceTypeParams(target)); len(typeParamDecls) > 0 {
			interfaceType.Types(typeParamDecls...)
		}
		interfaceType.InterfaceFunc(func(g *jen.Group) {
			g.Id(target.marker).Params()
			for _, accessor := range target.accessors {
				g.Id(accessor.getter).Params().Add(typeRenderer.code(accessor.fieldType))
				g.Id(accessor.setter).Params(typeRenderer.code(accessor.fieldType))
			}
		})
		f.Line()

		for _, st := range target.structs {
			f.Func().Params(typeRenderer.receiverType(st)).Id(target.marker).Params().Block()
			f.Line()
			for _, accessor := range target.accessors {
				fieldType := typeRenderer.code(accessor.fieldType)
				f.Func().Params(jen.Id("v").Add(typeRenderer.receiverType(st))).Id(accessor.getter).Params().Add(fieldType).Block(
					jen.Return(jen.Id("v").Dot(accessor.fieldName)),
				)
				f.Line()
				f.Func().Params(jen.Id("v").Add(typeRenderer.receiverType(st))).Id(accessor.setter).Params(jen.Id("value").Add(fieldType)).Block(
					jen.Id("v").Dot(accessor.fieldName).Op("=").Id("value"),
				)
				f.Line()
			}
		}
	}
	for _, target := range typeParamTargets {
		f.Type().Id(target.interfaceName).InterfaceFunc(func(g *jen.Group) {
			g.Id(target.marker).Params()
			for _, accessor := range target.accessors {
				g.Id(accessor.getter).Params().Add(typeRenderer.code(accessor.fieldType))
				g.Id(accessor.setter).Params(typeRenderer.code(accessor.fieldType))
			}
		})
		f.Line()

		for _, facet := range target.facets {
			f.Type().Id(facet.interfaceName).Types(
				jen.Id(facet.typeParam.name).Add(typeRenderer.code(facet.typeParam.constraint)),
			).InterfaceFunc(func(g *jen.Group) {
				g.Id(target.interfaceName)
				g.Id(facet.method).Params(jen.Id(facet.typeParam.name))
				for _, accessor := range facet.accessors {
					g.Id(accessor.getter).Params().Add(typeRenderer.code(accessor.fieldType))
					g.Id(accessor.setter).Params(typeRenderer.code(accessor.fieldType))
				}
			})
			f.Line()
		}
		for _, combination := range target.combinations {
			typeParamDecls := make([]jen.Code, 0, len(combination.facets))
			for _, facet := range combination.facets {
				typeParamDecls = append(typeParamDecls, jen.Id(facet.typeParam.name).Add(typeRenderer.code(facet.typeParam.constraint)))
			}
			f.Type().Id(combination.interfaceName).Types(typeParamDecls...).InterfaceFunc(func(g *jen.Group) {
				for _, facet := range combination.facets {
					g.Id(facet.interfaceName).Types(jen.Id(facet.typeParam.name))
				}
			})
			f.Line()
		}

		f.Func().Params(typeRenderer.receiverType(target.st)).Id(target.marker).Params().Block()
		f.Line()
		for _, accessor := range target.accessors {
			fieldType := typeRenderer.code(accessor.fieldType)
			f.Func().Params(jen.Id("v").Add(typeRenderer.receiverType(target.st))).Id(accessor.getter).Params().Add(fieldType).Block(
				jen.Return(jen.Id("v").Dot(accessor.fieldName)),
			)
			f.Line()
			f.Func().Params(jen.Id("v").Add(typeRenderer.receiverType(target.st))).Id(accessor.setter).Params(jen.Id("value").Add(fieldType)).Block(
				jen.Id("v").Dot(accessor.fieldName).Op("=").Id("value"),
			)
			f.Line()
		}
		for _, facet := range target.facets {
			f.Func().Params(typeRenderer.receiverType(target.st)).Id(facet.method).Params(jen.Id(facet.typeParam.name)).Block()
			f.Line()
			for _, accessor := range facet.accessors {
				fieldType := typeRenderer.code(accessor.fieldType)
				f.Func().Params(jen.Id("v").Add(typeRenderer.receiverType(target.st))).Id(accessor.getter).Params().Add(fieldType).Block(
					jen.Return(jen.Id("v").Dot(accessor.fieldName)),
				)
				f.Line()
				f.Func().Params(jen.Id("v").Add(typeRenderer.receiverType(target.st))).Id(accessor.setter).Params(jen.Id("value").Add(fieldType)).Block(
					jen.Id("v").Dot(accessor.fieldName).Op("=").Id("value"),
				)
				f.Line()
			}
		}
	}

	var b bytes.Buffer
	if err := f.Render(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func interfaceTypeParams(target generationTarget) []typeParamInfo {
	used := map[string]bool{}
	for _, accessor := range target.accessors {
		collectTypeParamNames(accessor.fieldType, used, map[types.Type]bool{})
	}
	if len(used) == 0 {
		return nil
	}

	params := target.structs[0].typeParams
	for changed := true; changed; {
		changed = false
		for _, param := range params {
			if !used[param.name] {
				continue
			}
			before := len(used)
			collectTypeParamNames(param.constraint, used, map[types.Type]bool{})
			if len(used) != before {
				changed = true
			}
		}
	}

	filtered := make([]typeParamInfo, 0, len(params))
	for _, param := range params {
		if used[param.name] {
			filtered = append(filtered, param)
		}
	}
	return filtered
}

func collectTypeParamNames(typ types.Type, names map[string]bool, seen map[types.Type]bool) {
	if typ == nil || seen[typ] {
		return
	}
	seen[typ] = true

	switch typ := typ.(type) {
	case *types.Alias:
		collectTypeParamNamesFromTypeList(typ.TypeArgs(), names, seen)
	case *types.Array:
		collectTypeParamNames(typ.Elem(), names, seen)
	case *types.Basic:
	case *types.Chan:
		collectTypeParamNames(typ.Elem(), names, seen)
	case *types.Map:
		collectTypeParamNames(typ.Key(), names, seen)
		collectTypeParamNames(typ.Elem(), names, seen)
	case *types.Interface:
		for etyp := range typ.EmbeddedTypes() {
			collectTypeParamNames(etyp, names, seen)
		}
		for method := range typ.ExplicitMethods() {
			collectTypeParamNames(method.Type(), names, seen)
		}
	case *types.Named:
		collectTypeParamNamesFromTypeList(typ.TypeArgs(), names, seen)
	case *types.Pointer:
		collectTypeParamNames(typ.Elem(), names, seen)
	case *types.Signature:
		collectTypeParamNamesFromTuple(typ.Params(), names, seen)
		collectTypeParamNamesFromTuple(typ.Results(), names, seen)
	case *types.Slice:
		collectTypeParamNames(typ.Elem(), names, seen)
	case *types.Struct:
		for i := range typ.NumFields() {
			collectTypeParamNames(typ.Field(i).Type(), names, seen)
		}
	case *types.TypeParam:
		names[typ.Obj().Name()] = true
	case *types.Union:
		for i := range typ.Len() {
			collectTypeParamNames(typ.Term(i).Type(), names, seen)
		}
	}
}

func collectTypeParamNamesFromTypeList(typesList *types.TypeList, names map[string]bool, seen map[types.Type]bool) {
	if typesList == nil {
		return
	}
	for typ := range typesList.Types() {
		collectTypeParamNames(typ, names, seen)
	}
}

func collectTypeParamNamesFromTuple(tuple *types.Tuple, names map[string]bool, seen map[types.Type]bool) {
	if tuple == nil {
		return
	}
	for i := range tuple.Len() {
		collectTypeParamNames(tuple.At(i).Type(), names, seen)
	}
}

func markerMethodName(interfaceName string) string {
	return "is" + interfaceName
}

type typeRenderer struct{}

func (r typeRenderer) receiverType(st structInfo) jen.Code {
	code := jen.Op("*").Id(st.name)
	if len(st.typeParams) == 0 {
		return code
	}
	return code.Types(r.typeParamArgs(st.typeParams)...)
}

func (r typeRenderer) typeParamArgs(params []typeParamInfo) []jen.Code {
	args := make([]jen.Code, 0, len(params))
	for _, param := range params {
		args = append(args, jen.Id(param.name))
	}
	return args
}

func (r typeRenderer) typeParamDecls(params []typeParamInfo) []jen.Code {
	decls := make([]jen.Code, 0, len(params))
	for _, param := range params {
		decls = append(decls, jen.Id(param.name).Add(r.code(param.constraint)))
	}
	return decls
}

func (r typeRenderer) code(typ types.Type) jen.Code {
	switch typ := typ.(type) {
	case *types.Alias:
		return r.namedType(typ.Obj(), typ.TypeArgs())
	case *types.Array:
		return jen.Index(jen.Lit(typ.Len())).Add(r.code(typ.Elem()))
	case *types.Basic:
		return jen.Id(typ.Name())
	case *types.Chan:
		return r.chanType(typ)
	case *types.Map:
		return jen.Map(r.code(typ.Key())).Add(r.code(typ.Elem()))
	case *types.Interface:
		return r.interfaceType(typ)
	case *types.Named:
		return r.namedType(typ.Obj(), typ.TypeArgs())
	case *types.Pointer:
		return jen.Op("*").Add(r.code(typ.Elem()))
	case *types.Signature:
		return r.signatureType(typ)
	case *types.Slice:
		return jen.Index().Add(r.code(typ.Elem()))
	case *types.Struct:
		return r.structType(typ)
	case *types.TypeParam:
		return jen.Id(typ.Obj().Name())
	case *types.Union:
		return r.unionType(typ)
	default:
		panic(fmt.Sprintf("unsupported type %T: %s", typ, typ.String()))
	}
}

func (r typeRenderer) interfaceType(typ *types.Interface) jen.Code {
	if typ.Empty() {
		return jen.Interface()
	}
	return jen.InterfaceFunc(func(g *jen.Group) {
		for etyp := range typ.EmbeddedTypes() {
			g.Add(r.code(etyp))
		}
		for method := range typ.ExplicitMethods() {
			sig := method.Type().(*types.Signature)
			code := jen.Id(method.Name()).Params(r.tupleTypes(sig.Params(), sig.Variadic())...)
			results := sig.Results()
			switch results.Len() {
			case 0:
			case 1:
				code.Add(r.code(results.At(0).Type()))
			default:
				code.Params(r.tupleTypes(results, false)...)
			}
			g.Add(code)
		}
	})
}

func (r typeRenderer) signatureType(sig *types.Signature) jen.Code {
	code := jen.Func().Params(r.tupleTypes(sig.Params(), sig.Variadic())...)
	results := sig.Results()
	switch results.Len() {
	case 0:
	case 1:
		code.Add(r.code(results.At(0).Type()))
	default:
		code.Params(r.tupleTypes(results, false)...)
	}
	return code
}

func (r typeRenderer) structType(typ *types.Struct) jen.Code {
	return jen.StructFunc(func(g *jen.Group) {
		for i := range typ.NumFields() {
			field := typ.Field(i)
			var code *jen.Statement
			if field.Embedded() {
				code = jen.Add(r.code(field.Type()))
			} else {
				code = jen.Id(field.Name()).Add(r.code(field.Type()))
			}
			if tag := typ.Tag(i); tag != "" {
				code.Add(jen.Op(structTagLiteral(tag)))
			}
			g.Add(code)
		}
	})
}

func structTagLiteral(tag string) string {
	if strconv.CanBackquote(tag) {
		return "`" + tag + "`"
	}
	return strconv.Quote(tag)
}

func (r typeRenderer) tupleTypes(tup *types.Tuple, variadic bool) []jen.Code {
	if tup == nil {
		return nil
	}
	codes := make([]jen.Code, 0, tup.Len())
	for i := range tup.Len() {
		typ := tup.At(i).Type()
		if variadic && i == tup.Len()-1 {
			if slice, ok := typ.(*types.Slice); ok {
				codes = append(codes, jen.Op("...").Add(r.code(slice.Elem())))
				continue
			}
		}
		codes = append(codes, r.code(typ))
	}
	return codes
}

func (r typeRenderer) namedType(obj *types.TypeName, args *types.TypeList) jen.Code {
	var code *jen.Statement
	if pkg := obj.Pkg(); pkg != nil {
		code = jen.Qual(pkg.Path(), obj.Name())
	} else {
		code = jen.Id(obj.Name())
	}
	if args == nil || args.Len() == 0 {
		return code
	}
	typeArgs := make([]jen.Code, 0, args.Len())
	for arg := range args.Types() {
		typeArgs = append(typeArgs, r.code(arg))
	}
	return code.Types(typeArgs...)
}

func (r typeRenderer) chanType(typ *types.Chan) jen.Code {
	switch typ.Dir() {
	case types.SendRecv:
		return jen.Chan().Add(r.code(typ.Elem()))
	case types.SendOnly:
		return jen.Chan().Op("<-").Add(r.code(typ.Elem()))
	case types.RecvOnly:
		return jen.Op("<-").Chan().Add(r.code(typ.Elem()))
	default:
		return jen.Chan().Add(r.code(typ.Elem()))
	}
}

func (r typeRenderer) unionType(typ *types.Union) jen.Code {
	var g jen.Statement
	for i := range typ.Len() {
		if i > 0 {
			g.Op("|")
		}
		term := typ.Term(i)
		if term.Tilde() {
			g.Op("~")
		}
		g.Add(r.code(term.Type()))
	}
	return &g
}
