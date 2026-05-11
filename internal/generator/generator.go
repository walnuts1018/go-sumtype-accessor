package generator

import (
	"bytes"
	"cmp"
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

	content, err := render(pkg.Name, pkg.PkgPath, targets)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Dir, cfg.Output), content, 0o644)
}

func loadPackage(dir string) (*packages.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedImports | packages.NeedDeps,
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
	var errCount int
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		errCount += len(pkg.Errors)
	})
	if errCount > 0 {
		errs := make([]error, errCount)
		i := 0
		packages.Visit(pkgs, nil, func(pkg *packages.Package) {
			for _, err := range pkg.Errors {
				errs[i] = err
				i++
			}
		})
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
				if _, ok := ts.Type.(*ast.StructType); !ok {
					continue
				}
				interfaceName := annotationValue(ts.Doc, gen.Doc)
				if interfaceName == "" {
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
				structsByInterface[interfaceName] = append(structsByInterface[interfaceName], structInfo{
					name:       ts.Name.Name,
					typeParams: typeParams(named),
					fields:     structFields(stType),
				})
			}
		}
	}
	for interfaceName := range structsByInterface {
		slices.SortFunc(structsByInterface[interfaceName], func(a, b structInfo) int {
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
	for field := range st.Fields() {
		if !field.Exported() {
			continue
		}
		fields[field.Name()] = fieldInfo{typ: field.Type()}
	}
	return fields
}

func annotationValue(groups ...*ast.CommentGroup) string {
	for _, group := range groups {
		if group == nil {
			continue
		}
		for line := range strings.SplitSeq(group.Text(), "\n") {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, annotationPrefix); ok {
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
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].name != b[i].name {
			return false
		}
		if !sameFieldType(a[i].constraint, b[i].constraint) {
			return false
		}
	}
	return true
}

func sameFieldType(a, b types.Type) bool {
	if types.Identical(a, b) {
		return true
	}
	return types.TypeString(a, nil) == types.TypeString(b, nil)
}

func render(packageName, packagePath string, targets []generationTarget) ([]byte, error) {
	f := jen.NewFile(packageName)
	f.HeaderComment("Code generated by go-sumtype-accessor; DO NOT EDIT.")

	typeRenderer := typeRenderer{packagePath: packagePath}
	for _, target := range targets {
		interfaceType := f.Type().Id(target.interfaceName)
		if typeParamDecls := typeRenderer.typeParamDecls(target.structs[0].typeParams); len(typeParamDecls) > 0 {
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
					jen.If(jen.Id("v").Op("==").Nil()).Block(
						jen.Var().Id("zero").Add(typeRenderer.code(accessor.fieldType)),
						jen.Return(jen.Id("zero")),
					),
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
	default:
		return jen.Id(types.TypeString(typ, r.qualifier))
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
