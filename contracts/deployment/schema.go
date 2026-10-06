package deployment

import (
	"encoding/json"
	"reflect"
	"strings"
)

// SchemaJSON is generated from the same closed types that the interpreter uses.
// The required codeflyRules vocabulary includes aggregate selector disjointness;
// a generic JSON Schema engine that lacks it is not a contract validator.
func SchemaJSON(context bool) []byte {
	typ := reflect.TypeFor[Inventory]()
	if context {
		typ = reflect.TypeFor[Context]()
	}
	s := typeSchema(typ)
	s["$schema"] = "https://codefly.dev/schemas/deployment/v1"
	s["codeflyRules"] = true
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}
func typeSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{typeSchema(t.Elem()), map[string]any{"type": "null"}}}
	}
	s := map[string]any{}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			props[name] = typeSchema(f.Type)
			if !strings.Contains(tag, ",omitempty") {
				required = append(required, name)
			} else if name == "release" {
				props[name] = typeSchema(f.Type.Elem())
			}
		}
		s["type"] = "object"
		s["properties"] = props
		s["required"] = required
		s["additionalProperties"] = false
	case reflect.Map:
		s["type"] = "object"
		s["additionalProperties"] = typeSchema(t.Elem())
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			s["type"] = "string"
			s["contentEncoding"] = "base64"
		} else {
			s["type"] = "array"
			s["items"] = typeSchema(t.Elem())
		}
	case reflect.String:
		s["type"] = "string"
	case reflect.Bool:
		s["type"] = "boolean"
	case reflect.Int64:
		s["type"] = "integer"
		s["minimum"] = -MaxInteger
		s["maximum"] = MaxInteger
	}

	if props, ok := s["properties"].(map[string]any); ok {
		property := func(name string) map[string]any { return props[name].(map[string]any) }
		switch t {
		case reflect.TypeFor[Inventory]():
			property("schema")["const"] = Schema
			property("complete")["const"] = true
			property("generation")["minimum"] = 1
		case reflect.TypeFor[Workload]():
			property("selector")["minProperties"] = 1
			property("credential_kind")["enum"] = []string{"none", "application", "delivery", "platform"}
		case reflect.TypeFor[Container]():
			property("image")["pattern"] = imagePattern.String()
		case reflect.TypeFor[Release]():
			property("publisher")["pattern"] = idPattern.String()
			property("name")["pattern"] = idPattern.String()
			property("version")["pattern"] = versionPattern.String()
		case reflect.TypeFor[PodSpec]():
			property("containers")["minItems"] = 1
		case reflect.TypeFor[Member](), reflect.TypeFor[Reference](), reflect.TypeFor[Previous](), reflect.TypeFor[Artifact]():
			property("digest")["pattern"] = digestPattern.String()
		}
	}
	return s
}
