package docparse

import (
	"errors"
	"fmt"
	"go/ast"
	"path"
	"strings"

	"github.com/teamwork/utils/goutil"
	"github.com/teamwork/utils/sliceutil"
)

// The Schema Object allows the definition of input and output data types.
type Schema struct {
	Reference   string   `json:"$ref,omitempty" yaml:"$ref,omitempty"`
	Title       string   `json:"title,omitempty" yaml:"title,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Type        string   `json:"type,omitempty" yaml:"type,omitempty"`
	Enum        []string `json:"enum,omitempty" yaml:"enum,omitempty"`
	Format      string   `json:"format,omitempty" yaml:"format,omitempty"`
	Required    []string `json:"required,omitempty" yaml:"required,omitempty"`

	// Store array items; for primitives:
	//   "items": {"type": "string"}
	// or custom types:
	//   "items": {"$ref": "#/definitions/positiveInteger"},
	Items *Schema `json:"items,omitempty" yaml:"items,omitempty"`

	// Store structs.
	Properties map[string]*Schema `json:"properties,omitempty" yaml:"properties,omitempty"`
}

// Convert a struct to a JSON schema.
func structToSchema(prog *Program, name string, ref Reference) (*Schema, error) {
	schema := &Schema{
		Title:       name,
		Description: ref.Info,
		Properties:  map[string]*Schema{},
	}

	for _, p := range ref.Fields {
		if p.KindField == nil {
			return nil, fmt.Errorf("p.KindField is nil for %v", name)
		}

		switch ref.Context {
		case "path", "query", "form":
			name = goutil.TagName(p.KindField, ref.Context)
		default:
			// TODO: doesn't have to be json tag; that's just what Desk happens to
			// use. We should get it from Content-Type or some such instead.
			name = goutil.TagName(p.KindField, "json")
		}

		if name == "-" {
			continue
		}
		if name == "" {
			name = p.Name
		}

		prop, err := fieldToSchema(prog, name, ref, p.KindField)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %v: %v", ref.Lookup, err)
		}

		// TODO: ugly
		if len(prop.Required) > 0 {
			switch ref.Context {
			case "path", "query", "form":
			// Do nothing
			default:
				name = goutil.TagName(p.KindField, ref.Context)
				schema.Required = append(schema.Required, name)
				prop.Required = nil
			}
		}

		if prop == nil {
			return nil, fmt.Errorf(
				"structToSchema: prop is nil for field %#v in %#v",
				name, ref.Lookup)
		}

		schema.Properties[name] = prop
	}

	return schema, nil
}

const (
	paramRequired  = "required"
	paramOptional  = "optional"
	paramOmitEmpty = "omitempty"
	paramReadOnly  = "readonly"
)

func setTags(name string, p *Schema, tags []string) error {
	for _, t := range tags {
		switch t {

		case paramRequired:
			p.Required = append(p.Required, name)
		case paramOptional:
			// Do nothing.
		// TODO: implement this (also load from struct tag?), but I
		// don't see any way to do that in the OpenAPI spec?
		case paramOmitEmpty:
			return fmt.Errorf("omitempty not implemented yet")
		// TODO
		case paramReadOnly:
			return fmt.Errorf("readonly not implemented yet")

		// Various string formats.
		// https://tools.ietf.org/html/draft-handrews-json-schema-validation-01#section-7.3
		case "date-time", "date", "time", "email", "idn-email", "hostname", "idn-hostname", "uri", "url":
			if t == "url" {
				t = "uri"
			}
			if t == "email" {
				t = "idn-email"
			}
			if t == "hostname" {
				t = "idn-hostname"
			}

			p.Format = t

		default:
			switch {
			case strings.HasPrefix(t, "enum: "):
				p.Type = "enum"
				for _, e := range strings.Split(t[5:], " ") {
					e = strings.TrimSpace(e)
					if e != "" {
						p.Enum = append(p.Enum, e)
					}
				}

			default:
				return fmt.Errorf("unknown parameter tag for %#v: %#v",
					name, t)
			}
		}
	}

	return nil
}

// Convert a struct field to JSON schema.
func fieldToSchema(prog *Program, fName string, ref Reference, f *ast.Field) (*Schema, error) {
	var p Schema

	if f.Doc != nil {
		p.Description = f.Doc.Text()
	} else if f.Comment != nil {
		p.Description = f.Comment.Text()
	}
	p.Description = strings.TrimSpace(p.Description)

	var tags []string
	p.Description, tags = parseTags(p.Description)
	_ = tags
	err := setTags(fName, &p, tags)
	if err != nil {
		return nil, err
	}

	pkg := ref.Package
	var name *ast.Ident

	dbg("fieldToSchema: %v", f.Names)

	sw := f.Type
start:
	switch typ := sw.(type) {

	// Don't support interface{} for now. We'd have to add a lot of complexity
	// for it, and not sure if we're ever going to need it.
	case *ast.InterfaceType:
		return nil, errors.New("fieldToSchema: interface{} is not supported")

	// Pointer type; we don't really care about this for now, so just read over
	// it.
	case *ast.StarExpr:
		sw = typ.X
		goto start

	// Simple identifiers such as "string", "int", "MyType", etc.
	case *ast.Ident:
		canon, err := canonicalType(ref.File, pkg, typ)
		if err != nil {
			return nil, fmt.Errorf("cannot get canonical type: %v", err)
		}
		if canon != nil {
			sw = canon
			goto start
		}

		p.Type = JSONSchemaType(typ.Name)

		// e.g. string, int64, etc.: don't need to look up.
		if isPrimitive(p.Type) {
			return &p, nil
		}

		p.Type = ""
		name = typ

	// An expression followed by a selector, e.g. "pkg.foo"
	case *ast.SelectorExpr:
		pkgSel, ok := typ.X.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf("typ.X is not ast.Ident: %#v", typ.X)
		}

		pkg = pkgSel.Name
		name = typ.Sel

		canon, err := canonicalType(ref.File, pkgSel.Name, typ.Sel)
		if err != nil {
			return nil, fmt.Errorf("cannot get canonical type: %v", err)
		}
		if canon != nil {
			sw = canon
			goto start
		}

		lookup := pkg + "." + name.Name
		t, f := MapType(lookup)

		p.Format = f
		if t != "" {
			p.Type = JSONSchemaType(t)
			return &p, nil
		}

		// Deal with array.
		// TODO: don't do this inline but at the end. Reason it doesn't work not
		// is because we always use GetReference().
		ts, _, _, err := findType(ref.File, pkg, name.Name)
		if err != nil {
			return nil, err
		}

		switch resolvType := ts.Type.(type) {
		case *ast.ArrayType:
			p.Type = "array"
			err := resolveArray(prog, ref, pkg, &p, resolvType.Elt)
			if err != nil {
				return nil, err
			}

			return &p, nil
		}

	// Maps
	case *ast.MapType:
		// As far as I can find there is no obvious/elegant way to represent
		// this in JSON schema, so simply don't support it for now. I don't
		// think we actually use this anywhere.
		// TODO: We should really support this...
		//return nil, errors.New("fieldToSchema: maps are not supported due to JSON schema limitations")
		p.Type = "object"

	// Array and slices.
	case *ast.ArrayType:
		p.Type = "array"

		err := resolveArray(prog, ref, pkg, &p, typ.Elt)
		if err != nil {
			return nil, err
		}

		return &p, nil

	default:
		return nil, fmt.Errorf("fieldToSchema: unknown type: %T", typ)
	}

	if name == nil {
		return &p, nil
	}

	// Check if the type resolves to a Go primitive.
	lookup := pkg + "." + name.Name
	t, err := getTypeInfo(prog, lookup, ref.File)
	if err != nil {
		return nil, err
	}
	if t != "" {
		p.Type = t
		if isPrimitive(p.Type) {
			return &p, nil
		}
	}

	if i := strings.LastIndex(lookup, "/"); i > -1 {
		lookup = pkg[i+1:] + "." + name.Name
	}

	p.Reference = lookup

	return &p, nil
}

func resolveArray(prog *Program, ref Reference, pkg string, p *Schema, typ ast.Expr) error {
	asw := typ

	var name *ast.Ident

arrayStart:
	switch typ := asw.(type) {

	// Ignore *
	case *ast.StarExpr:
		asw = typ.X
		goto arrayStart

	// Simple identifier: "string", "myCustomType".
	case *ast.Ident:

		dbg("resolveArray: ident: %#v", typ.Name)

		p.Items = &Schema{Type: JSONSchemaType(typ.Name)}

		if typ.Name == "byte" {
			p.Items = nil
			p.Type = "string"
			return nil
		}

		if isPrimitive(p.Items.Type) {
			return nil
		}

		p.Items.Type = ""
		name = typ

	// "pkg.foo"
	case *ast.SelectorExpr:

		dbg("resolveArray: selector: %#v -> %#v", typ.X, typ.Sel)

		pkgSel, ok := typ.X.(*ast.Ident)
		if !ok {
			return fmt.Errorf("typ.X is not ast.Ident: %#v", typ.X)
		}
		pkg = pkgSel.Name
		name = typ.Sel

	default:
		return fmt.Errorf("fieldToSchema: unknown array type: %T", typ)
	}

	// Check if the type resolves to a Go primitive.
	lookup := pkg + "." + name.Name
	t, err := getTypeInfo(prog, lookup, ref.File)
	if err != nil {
		return err
	}
	if t != "" {
		p.Type = t
		if isPrimitive(p.Type) {
			return nil
		}
	}

	if i := strings.LastIndex(pkg, "/"); i > -1 {
		lookup = pkg[i+1:] + "." + name.Name
	}
	p.Items = &Schema{Reference: lookup}

	// Add to prog.References.
	_, err = GetReference(prog, "", lookup, ref.File)
	return err
}

func isPrimitive(n string) bool {
	//"null", "boolean", "object", "array", "number", "string", "integer",
	return sliceutil.InStringSlice([]string{
		"null", "boolean", "number", "string", "integer",
	}, n)
}

var kindMap = map[string]string{
	//"":     "string",
	"int":     "integer",
	"int8":    "integer",
	"int16":   "integer",
	"int32":   "integer",
	"int64":   "integer",
	"uint8":   "integer",
	"uint16":  "integer",
	"uint32":  "integer",
	"uint64":  "integer",
	"float32": "number",
	"float64": "number",
	"bool":    "boolean",
	"byte":    "string",
	"rune":    "string",
	"error":   "string",
}

// JSONSchemaType gets the type name as used in JSON schema.
func JSONSchemaType(t string) string {
	if m, ok := kindMap[t]; ok {
		return m
	}
	return t
}

func getTypeInfo(prog *Program, lookup, filePath string) (string, error) {
	var name, pkg string
	if c := strings.LastIndex(lookup, "."); c > -1 {
		// imported path: models.Foo
		pkg = lookup[:c]
		name = lookup[c+1:]
	} else {
		// Current package: Foo
		pkg = path.Dir(filePath)
		name = lookup
	}

	// Find type.
	ts, _, _, err := findType(filePath, pkg, name)
	if err != nil {
		return "", err
	}

	ident, ok := ts.Type.(*ast.Ident)
	if !ok {
		return "", nil
	}

	t := JSONSchemaType(ident.Name)
	return t, nil
}

// Get the canonical type.
func canonicalType(currentFile, pkgPath string, typ *ast.Ident) (ast.Expr, error) {
	if builtInType(typ.Name) {
		return nil, nil
	}

	var ts *ast.TypeSpec
	if typ.Obj == nil {
		var err error
		ts, _, _, err = findType(currentFile, pkgPath, typ.Name)
		if err != nil {
			return nil, err
		}
	} else {
		ts = typ.Obj.Decl.(*ast.TypeSpec)
	}

	// Don't resolve structs; we do this later.
	if _, ok := ts.Type.(*ast.StructType); ok {
		return nil, nil
	}

	return ts.Type, nil
}