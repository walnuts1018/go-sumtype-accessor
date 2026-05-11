package generator

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/dave/jennifer/jen"
	"golang.org/x/tools/go/packages"
)

const annotationPrefix = "+go-sumtype-accessor="

type Config struct {
	Dir    string
	Output string
}

type structInfo struct {
	name   string
	fields map[string]fieldInfo
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

func Generate(cfg Config) error {
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Output == "" {
		cfg.Output = "sumtype_accessors.go"
	}

	pkg, err := loadPackage(cfg.Dir)
	if err != nil {
		return err
	}

	structsByInterface := map[string][]structInfo{}
	collectPackageInfo(pkg, structsByInterface)
	if len(structsByInterface) == 0 {
		return errors.New("no sumtype accessor annotations found")
	}

	var interfaceNames []string
	for interfaceName := range structsByInterface {
		interfaceNames = append(interfaceNames, interfaceName)
	}
	slices.Sort(interfaceNames)

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

	content, err := render(pkg.Name, pkg.PkgPath, targets)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Dir, cfg.Output), content, 0o644)
}

func loadPackage(dir string) (*packages.Package, error) {
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no go files found in %s", dir)
	}
	if len(pkgs[0].Errors) > 0 {
		errs := make([]error, 0, len(pkgs[0].Errors))
		for _, err := range pkgs[0].Errors {
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
	return pkgs[0], nil
}

func collectPackageInfo(pkg *packages.Package, structsByInterface map[string][]structInfo) {
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
					fields: structFields(pkg, typ),
				})
			}
		}
	}
	for interfaceName := range structsByInterface {
		slices.SortFunc(structsByInterface[interfaceName], func(a, b structInfo) int {
			return strings.Compare(a.name, b.name)
		})
	}
}

func structFields(pkg *packages.Package, st *ast.StructType) map[string]fieldInfo {
	fields := make(map[string]fieldInfo, len(st.Fields.List))
	for _, field := range st.Fields.List {
		if len(field.Names) != 1 {
			continue
		}
		name := field.Names[0]
		if !name.IsExported() {
			continue
		}
		typ := pkg.TypesInfo.TypeOf(field.Type)
		if typ == nil {
			continue
		}
		fields[name.Name] = fieldInfo{typ: typ}
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

	common := map[string]fieldInfo{}
	maps.Copy(common, structs[0].fields)
	for _, st := range structs[1:] {
		for name, field := range common {
			otherField, ok := st.fields[name]
			if !ok || !types.Identical(field.typ, otherField.typ) {
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

func render(packageName, packagePath string, targets []generationTarget) ([]byte, error) {
	f := jen.NewFile(packageName)
	f.HeaderComment("Code generated by go-sumtype-accessor; DO NOT EDIT.")

	typeRenderer := typeRenderer{packagePath: packagePath}
	for _, target := range targets {
		f.Type().Id(target.interfaceName).InterfaceFunc(func(g *jen.Group) {
			g.Id(target.marker).Params()
			for _, accessor := range target.accessors {
				g.Id(accessor.getter).Params().Add(typeRenderer.code(accessor.fieldType))
				g.Id(accessor.setter).Params(typeRenderer.code(accessor.fieldType))
			}
		})
		f.Line()

		for _, st := range target.structs {
			f.Func().Params(jen.Op("*").Id(st.name)).Id(target.marker).Params().Block()
			f.Line()
			for _, accessor := range target.accessors {
				fieldType := typeRenderer.code(accessor.fieldType)
				f.Func().Params(jen.Id("v").Op("*").Id(st.name)).Id(accessor.getter).Params().Add(fieldType).Block(
					jen.Return(jen.Id("v").Dot(accessor.fieldName)),
				)
				f.Line()
				f.Func().Params(jen.Id("v").Op("*").Id(st.name)).Id(accessor.setter).Params(jen.Id("value").Add(fieldType)).Block(
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

func markerMethodName(interfaceName string) string {
	return "is" + interfaceName
}

type typeRenderer struct {
	packagePath string
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
	default:
		return jen.Id(types.TypeString(typ, r.qualifier))
	}
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
		for i := 0; i < typ.NumFields(); i++ {
			field := typ.Field(i)
			var code *jen.Statement
			if field.Embedded() {
				code = jen.Add(r.code(field.Type()))
			} else {
				code = jen.Id(field.Name()).Add(r.code(field.Type()))
			}
			if tag := typ.Tag(i); tag != "" {
				code.Id(structTagLiteral(tag))
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
	for i := 0; i < tup.Len(); i++ {
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
	if pkg := obj.Pkg(); pkg != nil && pkg.Path() != r.packagePath {
		code = jen.Qual(pkg.Path(), obj.Name())
	} else {
		code = jen.Id(obj.Name())
	}
	if args.Len() == 0 {
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

func (r typeRenderer) qualifier(pkg *types.Package) string {
	if pkg.Path() == r.packagePath {
		return ""
	}
	return pkg.Name()
}
