package compiler

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"net/url"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/gopherjs/gopherjs/compiler/internal/analysis"
	"github.com/gopherjs/gopherjs/compiler/internal/typeparams"
	"github.com/gopherjs/gopherjs/compiler/typesutil"
)

// We use this character as a separator in synthetic identifiers instead of a
// regular dot. This character is safe for use in JS identifiers and helps to
// visually separate components of the name when it appears in a stack trace.
const midDot = "·"

// root returns the topmost function context corresponding to the package scope.
func (fc *funcContext) root() *funcContext {
	if fc.isRoot() {
		return fc
	}
	return fc.parent.root()
}

// isRoot returns true for the package-level context.
func (fc *funcContext) isRoot() bool {
	return fc.parent == nil
}

func (fc *funcContext) Write(b []byte) (int, error) {
	fc.writePos()
	fc.output = append(fc.output, b...)
	return len(b), nil
}

func (fc *funcContext) Printf(format string, values ...interface{}) {
	fc.Write([]byte(fc.Indentation(0)))
	fmt.Fprintf(fc, format, values...)
	fc.Write([]byte{'\n'})
	fc.Write(fc.delayedOutput)
	fc.delayedOutput = nil
}

func (fc *funcContext) PrintCond(cond bool, onTrue, onFalse string) {
	if !cond {
		fc.Printf("/* %s */ %s", strings.Replace(onTrue, "*/", "<star>/", -1), onFalse)
		return
	}
	fc.Printf("%s", onTrue)
}

func (fc *funcContext) SetPos(pos token.Pos) {
	fc.posAvailable = true
	fc.pos = pos
}

func (fc *funcContext) writePos() {
	if fc.posAvailable {
		fc.posAvailable = false
		fc.Write([]byte{'\b'})
		binary.Write(fc, binary.BigEndian, uint32(fc.pos))
	}
}

// Indented increases generated code indentation level by 1 for the code emitted
// from the callback f.
func (fc *funcContext) Indented(f func()) {
	fc.pkgCtx.indentation++
	f()
	fc.pkgCtx.indentation--
}

// Indentation returns a sequence of "\t" characters appropriate to the current
// generated code indentation level. The `extra` parameter provides relative
// indentation adjustment.
func (fc *funcContext) Indentation(extra int) string {
	return strings.Repeat("\t", fc.pkgCtx.indentation+extra)
}

func (fc *funcContext) CatchOutput(indent int, f func()) []byte {
	origoutput := fc.output
	fc.output = nil
	fc.pkgCtx.indentation += indent
	f()
	fc.writePos()
	caught := fc.output
	fc.output = origoutput
	fc.pkgCtx.indentation -= indent
	return caught
}

func (fc *funcContext) Delayed(f func()) {
	fc.delayedOutput = fc.CatchOutput(0, f)
}

// expandTupleArgs converts a function call which argument is a tuple returned
// by another function into a set of individual call arguments corresponding to
// tuple elements.
//
// For example, for functions defined as:
//
//	func a() (int, string) {return 42, "foo"}
//	func b(a1 int, a2 string) {}
//
// ...the following statement:
//
//	b(a())
//
// ...will be transformed into:
//
//	_tuple := a()
//	b(_tuple[0], _tuple[1])
func (fc *funcContext) expandTupleArgs(argExprs []ast.Expr) []ast.Expr {
	if len(argExprs) != 1 {
		return argExprs
	}

	tuple, isTuple := fc.typeOf(argExprs[0]).(*types.Tuple)
	if !isTuple {
		return argExprs
	}

	tupleVar := fc.newLocalVariable("_tuple")
	fc.Printf("%s = %s;", tupleVar, fc.translateExpr(argExprs[0]))
	argExprs = make([]ast.Expr, tuple.Len())
	for i := range argExprs {
		argExprs[i] = fc.newIdent(fc.formatExpr("%s[%d]", tupleVar, i).String(), tuple.At(i).Type())
	}
	return argExprs
}

func (fc *funcContext) translateArgs(sig *types.Signature, argExprs []ast.Expr, ellipsis bool) []string {
	argExprs = fc.expandTupleArgs(argExprs)

	sigTypes := typesutil.Signature{Sig: sig}

	if sig.Variadic() && len(argExprs) == 0 {
		return []string{fmt.Sprintf("%s.nil", fc.typeName(sigTypes.VariadicType()))}
	}

	preserveOrder := false
	for i := 1; i < len(argExprs); i++ {
		preserveOrder = preserveOrder || fc.Blocking[argExprs[i]]
	}

	args := make([]string, len(argExprs))
	for i, argExpr := range argExprs {
		arg := fc.translateImplicitConversionWithCloning(argExpr, sigTypes.Param(i, ellipsis)).String()

		if preserveOrder && fc.pkgCtx.Types[argExpr].Value == nil {
			argVar := fc.newLocalVariable("_arg")
			fc.Printf("%s = %s;", argVar, arg)
			arg = argVar
		}

		args[i] = arg
	}

	// If variadic arguments were passed in as individual elements, regroup them
	// into a slice and pass it as a single argument.
	if sig.Variadic() && !ellipsis {
		required := args[:sigTypes.RequiredParams()]
		var variadic string
		if len(args) == sigTypes.RequiredParams() {
			// If no variadic parameters were passed, the slice value defaults to nil.
			variadic = fmt.Sprintf("%s.nil", fc.typeName(sigTypes.VariadicType()))
		} else {
			variadic = fmt.Sprintf("new %s([%s])", fc.typeName(sigTypes.VariadicType()), strings.Join(args[sigTypes.RequiredParams():], ", "))
		}
		return append(required, variadic)
	}
	return args
}

func (fc *funcContext) translateSelection(sel typesutil.Selection, pos token.Pos) ([]string, string) {
	var fields []string
	t := sel.Recv()
	for _, index := range sel.Index() {
		if ptr, isPtr := t.Underlying().(*types.Pointer); isPtr {
			t = ptr.Elem()
		}
		s := t.Underlying().(*types.Struct)
		if jsTag := getJsTag(s.Tag(index)); jsTag != "" {
			jsFieldName := s.Field(index).Name()
			for {
				fields = append(fields, fieldName(s, 0))
				ft := fc.fieldType(s, 0)
				if typesutil.IsJsObject(ft) {
					return fields, jsTag
				}
				ft = ft.Underlying()
				if ptr, ok := ft.(*types.Pointer); ok {
					ft = ptr.Elem().Underlying()
				}
				var ok bool
				s, ok = ft.(*types.Struct)
				if !ok || s.NumFields() == 0 {
					fc.pkgCtx.errList = append(fc.pkgCtx.errList, types.Error{Fset: fc.pkgCtx.fileSet, Pos: pos, Msg: fmt.Sprintf("could not find field with type *js.Object for 'js' tag of field '%s'", jsFieldName), Soft: true})
					return nil, ""
				}
			}
		}
		fields = append(fields, fieldName(s, index))
		t = fc.fieldType(s, index)
	}
	return fields, ""
}

var nilObj = types.Universe.Lookup("nil")

func (fc *funcContext) zeroValue(ty types.Type) ast.Expr {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		switch {
		case isBoolean(t):
			return fc.newConst(ty, constant.MakeBool(false))
		case isNumeric(t):
			return fc.newConst(ty, constant.MakeInt64(0))
		case isString(t):
			return fc.newConst(ty, constant.MakeString(""))
		case t.Kind() == types.UnsafePointer:
			// fall through to "nil"
		case t.Kind() == types.UntypedNil:
			panic("Zero value for untyped nil.")
		default:
			panic(fmt.Sprintf("Unhandled basic type: %v\n", t))
		}
	case *types.Array, *types.Struct:
		return fc.setType(&ast.CompositeLit{}, ty)
	case *types.Chan, *types.Interface, *types.Map, *types.Signature, *types.Slice, *types.Pointer:
		// fall through to "nil"
	default:
		panic(fmt.Sprintf("Unhandled type: %T\n", t))
	}
	id := fc.newIdent("nil", ty)
	fc.pkgCtx.Uses[id] = nilObj
	return id
}

func (fc *funcContext) newConst(t types.Type, value constant.Value) ast.Expr {
	id := &ast.Ident{}
	fc.pkgCtx.Types[id] = types.TypeAndValue{Type: t, Value: value}
	return id
}

// newLocalVariable assigns a new JavaScript variable name for the given Go
// local variable name. In this context "local" means "in scope of the current"
// functionContext.
func (fc *funcContext) newLocalVariable(name string) string {
	return fc.newVariable(name, false)
}

// newVariable assigns a new JavaScript variable name for the given Go variable
// or type.
//
// If there is already a variable with the same name visible in the current
// function context (e.g. due to shadowing), the returned name will be suffixed
// with a number to prevent conflict. This is necessary because Go name
// resolution scopes differ from var declarations in JS.
//
// If pkgLevel is true, the variable is declared at the package level and added
// to this functionContext, as well as all parents, but not to the list of local
// variables. If false, it is added to this context only, as well as the list of
// local vars.
func (fc *funcContext) newVariable(name string, pkgLevel bool) string {
	if name == "" {
		panic("newVariable: empty name")
	}
	name = encodeIdent(name)
	if fc.pkgCtx.minify {
		i := 0
		for {
			offset := int('a')
			if pkgLevel {
				offset = int('A')
			}
			j := i
			name = ""
			for {
				name = string(rune(offset+(j%26))) + name
				j = j/26 - 1
				if j == -1 {
					break
				}
			}
			if fc.allVars[name] == 0 {
				break
			}
			i++
		}
	}
	n := fc.allVars[name]
	fc.allVars[name] = n + 1
	varName := name
	if n > 0 {
		varName = fmt.Sprintf("%s$%d", name, n)
	}

	if pkgLevel {
		for c2 := fc.parent; c2 != nil; c2 = c2.parent {
			c2.allVars[name] = n + 1
		}
		return varName
	}

	fc.localVars = append(fc.localVars, varName)
	return varName
}

// newIdent declares a new Go variable with the given name and type and returns
// an *ast.Ident referring to that object.
func (fc *funcContext) newIdent(name string, t types.Type) *ast.Ident {
	obj := types.NewVar(0, fc.pkgCtx.Pkg, name, t)
	fc.objectNames[obj] = name
	return fc.newIdentFor(obj)
}

// newIdentFor creates a new *ast.Ident referring to the given Go object.
func (fc *funcContext) newIdentFor(obj types.Object) *ast.Ident {
	ident := ast.NewIdent(obj.Name())
	ident.NamePos = obj.Pos()
	fc.pkgCtx.Uses[ident] = obj
	fc.setType(ident, obj.Type())
	return ident
}

func (fc *funcContext) newTypeIdent(name string, obj types.Object) *ast.Ident {
	ident := ast.NewIdent(name)
	fc.pkgCtx.Info.Uses[ident] = obj
	return ident
}

// newLitFuncName generates a new synthetic name for a function literal.
func (fc *funcContext) newLitFuncName() string {
	fc.funcLitCounter++
	name := &strings.Builder{}

	// If function literal is defined inside another function, qualify its
	// synthetic name with the outer function to make it easier to identify.
	if fc.instance.Object != nil {
		if recvType := typesutil.RecvType(fc.sig.Sig); recvType != nil {
			name.WriteString(recvType.Obj().Name())
			name.WriteString(midDot)
		}
		name.WriteString(fc.instance.Object.Name())
		name.WriteString(midDot)
	}
	fmt.Fprintf(name, "func%d", fc.funcLitCounter)
	return name.String()
}

func (fc *funcContext) setType(e ast.Expr, t types.Type) ast.Expr {
	fc.pkgCtx.Types[e] = types.TypeAndValue{Type: t}
	return e
}

func (fc *funcContext) pkgVar(pkg *types.Package) string {
	if pkg == fc.pkgCtx.Pkg {
		return "$pkg"
	}

	pkgVar, found := fc.pkgCtx.pkgVars[pkg.Path()]
	if !found {
		pkgVar = fmt.Sprintf(`$packages["%s"]`, pkg.Path())
	}
	return pkgVar
}

func isVarOrConst(o types.Object) bool {
	switch o.(type) {
	case *types.Var, *types.Const:
		return true
	}
	return false
}

func isPkgLevel(o types.Object) bool {
	// Note: named types are always assigned a variable at package level to be
	// initialized with the rest of the package types, even the types declared
	// in a statement inside a function.
	_, isType := o.(*types.TypeName)
	return (o.Parent() != nil && o.Parent().Parent() == types.Universe) || isType
}

// assignedObjectName checks if the object has been previously assigned a name
// in this or one of the parent contexts. If not, found will be false.
func (fc *funcContext) assignedObjectName(o types.Object) (name string, found bool) {
	if fc == nil {
		return "", false
	}
	if name, found := fc.parent.assignedObjectName(o); found {
		return name, true
	}

	name, found = fc.objectNames[o]
	return name, found
}

// objectName returns a JS expression that refers to the given object. If the
// object hasn't been previously assigned a JS variable name, it will be
// allocated as needed.
func (fc *funcContext) objectName(o types.Object) string {
	if isPkgLevel(o) {
		var nestTArgs []types.Type
		if typeparams.FindNestingFunc(o) == fc.instance.Object {
			// Only set the nest type arguments for objects nested in this funcContext.
			nestTArgs = fc.instance.TArgs
		}
		fc.pkgCtx.DeclareDCEDep(o, nestTArgs, nil)

		if o.Pkg() != fc.pkgCtx.Pkg || (isVarOrConst(o) && o.Exported()) {
			return fc.pkgVar(o.Pkg()) + "." + o.Name()
		}
	}

	name, ok := fc.assignedObjectName(o)
	if !ok {
		pkgLevel := isPkgLevel(o)
		name = fc.newVariable(o.Name(), pkgLevel)
		if pkgLevel {
			fc.root().objectNames[o] = name
		} else {
			fc.objectNames[o] = name
		}
	}

	if v, ok := o.(*types.Var); ok && fc.pkgCtx.escapingVars[v] {
		return name + "[0]"
	}
	return name
}

// knownInstances returns a list of known instantiations of the object.
//
// For objects without type params and not nested in a generic function or
// method, this always returns a single trivial instance.
// If the object is generic, or in a generic function or method, but there are
// no instances, then the object is unused and an empty list is returned.
func (fc *funcContext) knownInstances(o types.Object) []typeparams.Instance {
	instances := fc.pkgCtx.instanceSet.Pkg(o.Pkg()).ForObj(o)
	if len(instances) == 0 && !typeparams.HasTypeParams(o.Type()) {
		return []typeparams.Instance{{Object: o}}
	}
	return instances
}

// instName returns a JS expression that refers to the provided instance of a
// function or type. Non-generic objects may be represented as an instance with
// zero type arguments.
func (fc *funcContext) instName(inst typeparams.Instance) string {
	objName := fc.objectName(inst.Object)
	if inst.IsTrivial() {
		return objName
	}
	fc.pkgCtx.DeclareDCEDep(inst.Object, inst.TNest, inst.TArgs)
	label := inst.TypeParamsString(` /* `, ` */`)
	return fmt.Sprintf("%s[%d%s]", objName, fc.pkgCtx.instanceSet.ID(inst), label)
}

// methodName returns a JS identifier (specifically, object property name)
// corresponding to the given method.
func (fc *funcContext) methodName(fun *types.Func) string {
	if fun.Type().(*types.Signature).Recv() == nil {
		panic(fmt.Errorf("expected a method, got a standalone function %v", fun))
	}
	name := fun.Name()
	// Method names are scoped to their receiver type and guaranteed to be
	// unique within that, so we only need to make sure it's not a reserved keyword
	if reservedKeywords[name] {
		name += "$"
	}
	return name
}

func (fc *funcContext) varPtrName(o *types.Var) string {
	if isPkgLevel(o) && o.Exported() {
		return fc.pkgVar(o.Pkg()) + "." + o.Name() + "$ptr"
	}

	name, ok := fc.pkgCtx.varPtrNames[o]
	if !ok {
		name = fc.newVariable(o.Name()+"$ptr", isPkgLevel(o))
		fc.pkgCtx.varPtrNames[o] = name
	}
	return name
}

// typeName returns a JS identifier name for the given Go type.
//
// For the built-in types it returns identifiers declared in the prelude. For
// all user-defined or composite types it creates a unique JS identifier and
// will return it on all subsequent calls for the type.
func (fc *funcContext) typeName(ty types.Type) string {
	switch t := ty.(type) {
	case *types.Basic:
		return "$" + toJavaScriptType(t)
	case *types.Named:
		if t.Obj().Name() == "error" {
			return "$error"
		}
		inst := typeparams.Instance{Object: t.Obj()}

		// Get type arguments for the type if there are any.
		for i := 0; i < t.TypeArgs().Len(); i++ {
			inst.TArgs = append(inst.TArgs, t.TypeArgs().At(i))
		}

		// Get the nesting type arguments if there are any.
		if fn := typeparams.FindNestingFunc(t.Obj()); fn != nil {
			if fn.Scope().Contains(t.Obj().Pos()) {
				tp := typeparams.SignatureTypeParams(fn.Type().(*types.Signature))
				tNest := make([]types.Type, tp.Len())
				for i := 0; i < tp.Len(); i++ {
					tNest[i] = fc.typeResolver.Substitute(tp.At(i))
				}
				inst.TNest = typesutil.TypeList(tNest)
			}
		}

		return fc.instName(inst)
	case *types.Interface:
		if t.Empty() {
			return "$emptyInterface"
		}
	case *types.TypeParam:
		panic(fmt.Errorf("unexpected type parameter: %v", t))
	}

	// For anonymous composite types, generate a synthetic package-level type
	// declaration, which will be reused for all instances of this type. This
	// improves performance, since runtime won't have to synthesize the same type
	// repeatedly.
	anonType, ok := fc.pkgCtx.anonTypeMap.At(ty).(*types.TypeName)
	if !ok {
		fc.initArgs(ty) // cause all embedded types to be registered
		varName := fc.newVariable(strings.ToLower(typeKind(ty)[5:])+"Type", true)
		anonType = types.NewTypeName(token.NoPos, fc.pkgCtx.Pkg, varName, ty) // fake types.TypeName
		fc.pkgCtx.anonTypes = append(fc.pkgCtx.anonTypes, anonType)
		fc.pkgCtx.anonTypeMap.Set(ty, anonType)
	}
	// Since anonymous types are always package-level so they can be shared,
	// don't pass in the function context (nest type parameters) to the DCE.
	fc.pkgCtx.DeclareDCEDep(anonType, nil, nil)
	return anonType.Name()
}

// importedPkgVar returns a package-level variable name for accessing an imported
// package.
//
// Allocates a new variable if this is the first call, or returns the existing
// one. The variable is based on the package name (implicitly derived from the
// `package` declaration in the imported package, or explicitly assigned by the
// import decl in the importing source file).
//
// Returns the allocated variable name.
func (fc *funcContext) importedPkgVar(pkg *types.Package) string {
	if pkgVar, ok := fc.pkgCtx.pkgVars[pkg.Path()]; ok {
		return pkgVar // Already registered.
	}

	pkgVar := fc.newVariable(pkg.Name(), true)
	fc.pkgCtx.pkgVars[pkg.Path()] = pkgVar
	return pkgVar
}

// instanceOf constructs an instance description of the object the ident is
// referring to. For non-generic objects, it will return a trivial instance with
// no type arguments.
func (fc *funcContext) instanceOf(ident *ast.Ident) typeparams.Instance {
	inst := typeparams.Instance{Object: fc.pkgCtx.ObjectOf(ident)}
	if i, ok := fc.pkgCtx.Instances[ident]; ok {
		inst.TArgs = fc.typeResolver.SubstituteAll(i.TypeArgs)
	}
	return inst
}

// typeOf returns a type associated with the given AST expression. For types
// defined in terms of type parameters, it will substitute type parameters with
// concrete types from the current set of type arguments.
func (fc *funcContext) typeOf(expr ast.Expr) types.Type {
	typ := fc.pkgCtx.TypeOf(expr)
	// If the expression is referring to an instance of a generic type or function,
	// we want the instantiated type.
	if ident, ok := expr.(*ast.Ident); ok {
		if inst, ok := fc.pkgCtx.Instances[ident]; ok {
			typ = inst.Type
		}
	}
	return fc.typeResolver.Substitute(typ)
}

// fieldType returns the type of the i-th field of the given struct
// after substituting type parameters with concrete types for nested context.
func (fc *funcContext) fieldType(t *types.Struct, i int) types.Type {
	return fc.typeResolver.Substitute(t.Field(i).Type())
}

func (fc *funcContext) selectionOf(e *ast.SelectorExpr) (typesutil.Selection, bool) {
	if sel, ok := fc.pkgCtx.Selections[e]; ok {
		return fc.typeResolver.SubstituteSelection(sel), true
	}
	if sel, ok := fc.pkgCtx.additionalSelections[e]; ok {
		return sel, true
	}
	return nil, false
}

func (fc *funcContext) externalize(s string, t types.Type) string {
	if typesutil.IsJsObject(t) {
		return s
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		if isNumeric(u) && !is64Bit(u) && !isComplex(u) {
			return s
		}
		if u.Kind() == types.UntypedNil {
			return "null"
		}
	}
	return fmt.Sprintf("$externalize(%s, %s)", s, fc.typeName(t))
}

func (fc *funcContext) handleEscapingVars(n ast.Node) {
	newEscapingVars := make(map[*types.Var]bool)
	for escaping := range fc.pkgCtx.escapingVars {
		newEscapingVars[escaping] = true
	}
	fc.pkgCtx.escapingVars = newEscapingVars

	var names []string
	objs := analysis.EscapingObjects(n, fc.pkgCtx.Info.Info)
	for _, obj := range objs {
		names = append(names, fc.objectName(obj))
		fc.pkgCtx.escapingVars[obj] = true
	}
	sort.Strings(names)
	for _, name := range names {
		fc.Printf("%s = [%s];", name, name)
	}
}

func fieldName(t *types.Struct, i int) string {
	name := t.Field(i).Name()
	if name == "_" || reservedKeywords[name] {
		return fmt.Sprintf("%s$%d", name, i)
	}
	return name
}

func typeKind(ty types.Type) string {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		return "$kind" + toJavaScriptType(t)
	case *types.Array:
		return "$kindArray"
	case *types.Chan:
		return "$kindChan"
	case *types.Interface:
		return "$kindInterface"
	case *types.Map:
		return "$kindMap"
	case *types.Signature:
		return "$kindFunc"
	case *types.Slice:
		return "$kindSlice"
	case *types.Struct:
		return "$kindStruct"
	case *types.Pointer:
		return "$kindPtr"
	default:
		panic(fmt.Sprintf("Unhandled type: %T\n", t))
	}
}

func toJavaScriptType(t *types.Basic) string {
	switch t.Kind() {
	case types.UntypedInt:
		return "Int"
	case types.Byte:
		return "Uint8"
	case types.Rune:
		return "Int32"
	case types.UnsafePointer:
		return "UnsafePointer"
	default:
		name := t.String()
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

func is64Bit(t *types.Basic) bool {
	return t.Kind() == types.Int64 || t.Kind() == types.Uint64
}

func isBoolean(t *types.Basic) bool {
	return t.Info()&types.IsBoolean != 0
}

func isComplex(t *types.Basic) bool {
	return t.Info()&types.IsComplex != 0
}

func isFloat(t *types.Basic) bool {
	return t.Info()&types.IsFloat != 0
}

func isInteger(t *types.Basic) bool {
	return t.Info()&types.IsInteger != 0
}

func isNumeric(t *types.Basic) bool {
	return t.Info()&types.IsNumeric != 0
}

func isString(t *types.Basic) bool {
	return t.Info()&types.IsString != 0
}

func isUnsigned(t *types.Basic) bool {
	return t.Info()&types.IsUnsigned != 0
}

func isBlank(expr ast.Expr) bool {
	if expr == nil {
		return true
	}
	if id, isIdent := expr.(*ast.Ident); isIdent {
		return id.Name == "_"
	}
	return false
}

// isWrapped returns true for types that may need to be boxed to access full
// functionality of the Go type.
//
// For efficiency or interoperability reasons certain Go types can be represented
// by JavaScript values that weren't constructed by the corresponding Go type
// constructor.
//
// For example, consider a Go type:
//
//	type SecretInt int
//	func (_ SecretInt) String() string { return "<secret>" }
//
//	func main() {
//	  var i SecretInt = 1
//	  println(i.String())
//	}
//
// For this example the compiler will generate code similar to the snippet below:
//
//	SecretInt = $pkg.SecretInt = $newType(4, $kindInt, "main.SecretInt", true, "main", true, null);
//	SecretInt.prototype.String = function() {
//	  return "<secret>";
//	};
//	main = function() {
//	  var i = 1;
//	  console.log(new SecretInt(i).String());
//	};
//
// Note that the generated code assigns a primitive "number" value into i, and
// only boxes it into an object when it's necessary to access its methods.
func isWrapped(ty types.Type) bool {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		return !is64Bit(t) && !isComplex(t) && t.Kind() != types.UntypedNil
	case *types.Array, *types.Chan, *types.Map, *types.Signature:
		return true
	case *types.Pointer:
		_, isArray := t.Elem().Underlying().(*types.Array)
		return isArray
	}
	return false
}

func encodeString(s string) string {
	buffer := bytes.NewBuffer(nil)
	for _, r := range []byte(s) {
		switch r {
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		case '\v':
			buffer.WriteString(`\v`)
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		default:
			if r < 0x20 || r > 0x7E {
				fmt.Fprintf(buffer, `\x%02X`, r)
				continue
			}
			buffer.WriteByte(r)
		}
	}
	return `"` + buffer.String() + `"`
}

func getJsTag(tag string) string {
	for tag != "" {
		// skip leading space
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}

		// scan to colon.
		// a space or a quote is a syntax error
		i = 0
		for i < len(tag) && tag[i] != ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := string(tag[:i])
		tag = tag[i+1:]

		// scan quoted string to find value
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		qvalue := string(tag[:i+1])
		tag = tag[i+1:]

		if name == "js" {
			value, _ := strconv.Unquote(qvalue)
			return value
		}
	}
	return ""
}

func needsSpace(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$'
}

func removeWhitespace(b []byte, minify bool) []byte {
	if !minify {
		return b
	}

	var out []byte
	var previous byte
	for len(b) > 0 {
		switch b[0] {
		case '\b':
			out = append(out, b[:5]...)
			b = b[5:]
			continue
		case ' ', '\t', '\n':
			if (!needsSpace(previous) || !needsSpace(b[1])) && !(previous == '-' && b[1] == '-') {
				b = b[1:]
				continue
			}
		case '"':
			out = append(out, '"')
			b = b[1:]
			for {
				i := bytes.IndexAny(b, "\"\\")
				out = append(out, b[:i]...)
				b = b[i:]
				if b[0] == '"' {
					break
				}
				// backslash
				out = append(out, b[:2]...)
				b = b[2:]
			}
		case '/':
			if b[1] == '*' {
				i := bytes.Index(b[2:], []byte("*/"))
				b = b[i+4:]
				continue
			}
		}
		out = append(out, b[0])
		previous = b[0]
		b = b[1:]
	}
	return out
}

func rangeCheck(pattern string, constantIndex, array bool) string {
	if constantIndex && array {
		return pattern
	}
	lengthProp := "$length"
	if array {
		lengthProp = "length"
	}
	check := "%2f >= %1e." + lengthProp
	if !constantIndex {
		check = "(%2f < 0 || " + check + ")"
	}
	return "(" + check + ` ? ($throwRuntimeError("index out of range"), undefined) : ` + pattern + ")"
}

func encodeIdent(name string) string {
	// Quick-and-dirty way to make any string safe for use as an identifier in JS.
	name = url.QueryEscape(name)
	// We use unicode middle dot as a visual separator in synthetic identifiers.
	// It is safe for use in a JS identifier, so we un-encode it for readability.
	name = strings.ReplaceAll(name, "%C2%B7", midDot)
	// QueryEscape uses '%' before hex-codes of escaped characters, which is not
	// allowed in a JS identifier, use '$' instead.
	name = strings.ReplaceAll(name, "%", "$")
	return name
}

// formatJSStructTagVal returns JavaScript code for accessing an object's property
// identified by jsTag. It prefers the dot notation over the bracket notation when
// possible, since the dot notation produces slightly smaller output.
//
// For example:
//
//	"my_name" -> ".my_name"
//	"my name" -> `["my name"]`
//
// For more information about JavaScript property accessors and identifiers, see
// https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Operators/Property_Accessors and
// https://developer.mozilla.org/en-US/docs/Glossary/Identifier.
func formatJSStructTagVal(jsTag string) string {
	for i, r := range jsTag {
		ok := unicode.IsLetter(r) || (i != 0 && unicode.IsNumber(r)) || r == '$' || r == '_'
		if !ok {
			// Saw an invalid JavaScript identifier character,
			// so use bracket notation.
			return `["` + template.JSEscapeString(jsTag) + `"]`
		}
	}
	// Safe to use dot notation without any escaping.
	return "." + jsTag
}

// FatalError is an error compiler panics with when it encountered a fatal error.
//
// FatalError implements io.Writer, which can be used to record any free-form
// debugging details for human consumption. This information will be included
// into String() result along with the rest.
type FatalError struct {
	cause interface{}
	stack []byte
	clues strings.Builder
}

func (b FatalError) Unwrap() error {
	if b.cause == nil {
		return nil
	}
	if err, ok := b.cause.(error); ok {
		return err
	}
	if s, ok := b.cause.(string); ok {
		return errors.New(s)
	}
	return fmt.Errorf("[%T]: %v", b.cause, b.cause)
}

// Write implements io.Writer and can be used to store free-form debugging clues.
func (b *FatalError) Write(p []byte) (n int, err error) { return b.clues.Write(p) }

func (b FatalError) Error() string {
	buf := &strings.Builder{}
	fmt.Fprintln(buf, "[compiler panic] ", strings.TrimSpace(b.Unwrap().Error()))
	if b.clues.Len() > 0 {
		fmt.Fprintln(buf, "\n"+b.clues.String())
	}
	if len(b.stack) > 0 {
		// Shift stack track by 2 spaces for better readability.
		stack := regexp.MustCompile("(?m)^").ReplaceAll(b.stack, []byte("  "))
		fmt.Fprintln(buf, "\nOriginal stack trace:\n", string(stack))
	}
	return buf.String()
}

func bailout(cause interface{}) *FatalError {
	b := &FatalError{
		cause: cause,
		stack: debug.Stack(),
	}
	return b
}

func bailingOut(err interface{}) (*FatalError, bool) {
	fe, ok := err.(*FatalError)
	return fe, ok
}

func removeMatching[T comparable](haystack []T, needle T) []T {
	var result []T
	for _, el := range haystack {
		if el != needle {
			result = append(result, el)
		}
	}
	return result
}
