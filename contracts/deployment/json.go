package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Violation is the stable machine interface; Message is diagnostic only.
type Violation struct {
	Rule    string `json:"rule"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (v *Violation) Error() string            { return v.Rule + " at " + v.Path + ": " + v.Message }
func refuse(rule, path, message string) error { return &Violation{rule, path, message} }

const MaxInputBytes = 16 << 20
const MaxDepth = 64
const MaxInteger int64 = 9007199254740991

// CanonicalJSON uses the frozen codefly-json-v1 encoding, not RFC 8785. Only
// integers within the exact interoperable range are supported. Arrays retain
// order. Object keys sort by UTF-8 bytes. Strings use Go JSON escaping (including
// HTML and U+2028/U+2029 escapes). There is no trailing newline or Unicode folding.
func CanonicalJSON(input []byte) ([]byte, error) {
	value, err := parseJSON(input)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func parseJSON(input []byte) (any, error) {
	if len(input) > MaxInputBytes {
		return nil, refuse("JSON_LIMIT", "$", "input exceeds 16 MiB")
	}
	if !utf8.Valid(input) {
		return nil, refuse("JSON_UNICODE", "$", "invalid UTF-8")
	}
	// encoding/json replaces unpaired UTF-16 surrogates. Reject them before decode.
	for i := 0; i < len(input); i++ {
		if input[i] != '"' {
			continue
		}
		i++
		for ; i < len(input) && input[i] != '"'; i++ {
			if input[i] != '\\' {
				continue
			}
			i++
			if i >= len(input) || input[i] != 'u' {
				continue
			}
			if i+4 >= len(input) {
				break
			}
			n, err := strconv.ParseUint(string(input[i+1:i+5]), 16, 16)
			if err != nil {
				break
			}
			i += 4
			if n >= 0xdc00 && n <= 0xdfff {
				return nil, refuse("JSON_UNICODE", "$", "unpaired surrogate")
			}
			if n >= 0xd800 && n <= 0xdbff {
				if i+6 >= len(input) || string(input[i+1:i+3]) != "\\u" {
					return nil, refuse("JSON_UNICODE", "$", "unpaired surrogate")
				}
				low, e := strconv.ParseUint(string(input[i+3:i+7]), 16, 16)
				if e != nil || low < 0xdc00 || low > 0xdfff {
					return nil, refuse("JSON_UNICODE", "$", "unpaired surrogate")
				}
				i += 6
			}
		}
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	value, err := readValue(dec, "$", 0)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, refuse("JSON_SYNTAX", "$", "trailing data")
	}
	return value, nil
}
func readValue(dec *json.Decoder, path string, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, refuse("JSON_LIMIT", path, "nesting exceeds 64")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, refuse("JSON_SYNTAX", path, "invalid JSON")
	}
	switch x := tok.(type) {
	case json.Delim:
		switch x {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return nil, refuse("JSON_SYNTAX", path, "invalid key")
				}
				key, ok := k.(string)
				if !ok {
					return nil, refuse("JSON_SYNTAX", path, "invalid key")
				}
				if _, ok = obj[key]; ok {
					return nil, refuse("JSON_DUPLICATE", path+"/"+key, "duplicate key")
				}
				obj[key], e = readValue(dec, path+"/"+key, depth+1)
				if e != nil {
					return nil, e
				}
			}
			if end, e := dec.Token(); e != nil || end != json.Delim('}') {
				return nil, refuse("JSON_SYNTAX", path, "unclosed object")
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, e := readValue(dec, fmt.Sprintf("%s/%d", path, len(arr)), depth+1)
				if e != nil {
					return nil, e
				}
				arr = append(arr, v)
			}
			if end, e := dec.Token(); e != nil || end != json.Delim(']') {
				return nil, refuse("JSON_SYNTAX", path, "unclosed array")
			}
			return arr, nil
		}
		return nil, refuse("JSON_SYNTAX", path, "unexpected delimiter")
	case json.Number:
		s := string(x)
		n, e := strconv.ParseInt(s, 10, 64)
		if e != nil || n > MaxInteger || n < -MaxInteger || s == "-0" {
			return nil, refuse("JSON_NUMBER", path, "only canonical integers in [-9007199254740991,9007199254740991] are supported")
		}
		return x, nil
	default:
		return tok, nil
	}
}
func decode[T any](input []byte) (T, error) {
	var out T
	value, err := parseJSON(input)
	if err != nil {
		return out, err
	}
	if err = shape(value, reflect.TypeFor[T](), "$"); err != nil {
		return out, err
	}
	if err = json.Unmarshal(input, &out); err != nil {
		return out, refuse("SCHEMA_TYPE", "$", err.Error())
	}
	return out, nil
}
func shape(value any, typ reflect.Type, path string) error {
	if typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return shape(value, typ.Elem(), path)
	}
	if value == nil {
		return refuse("SCHEMA_REQUIRED", path, "null is not an explicit collection or value")
	}
	switch typ.Kind() {
	case reflect.Struct:
		obj, ok := value.(map[string]any)
		if !ok {
			break
		}
		fields := map[string]reflect.StructField{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := fields[k]; !ok {
				return refuse("SCHEMA_UNKNOWN", path+"/"+k, "unknown field")
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			v, ok := obj[key]
			if !ok {
				if strings.Contains(f.Tag.Get("json"), ",omitempty") {
					continue
				}
				return refuse("SCHEMA_REQUIRED", path+"/"+key, "field must be explicit")
			}
			if key == "release" && v == nil {
				return refuse("RELEASE_METADATA", path+"/release", "present release must be an object")
			}
			if err := shape(v, f.Type, path+"/"+key); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		obj, ok := value.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := shape(obj[k], typ.Elem(), path+"/"+k); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			if _, ok := value.(string); ok {
				return nil
			}
			break
		}
		arr, ok := value.([]any)
		if !ok {
			break
		}
		for i, v := range arr {
			if err := shape(v, typ.Elem(), fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		if _, ok := value.(string); ok {
			return nil
		}
	case reflect.Bool:
		if _, ok := value.(bool); ok {
			return nil
		}
	case reflect.Int64:
		if _, ok := value.(json.Number); ok {
			return nil
		}
	}
	return refuse("SCHEMA_TYPE", path, "unexpected JSON type")
}
