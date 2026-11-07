package bencode

import (
	"bytes"
	"reflect"
	"sort"
	"strconv"
)

var rawType = reflect.TypeOf(RawMessage(nil))

// Marshal encodes v canonically: dict keys sorted, struct fields emitted in
// key order. Supported: string, []byte, byte arrays, bool (as 0/1), all
// int/uint kinds, slices/arrays, map[string]T, structs (tags in the package
// doc), pointers, interfaces, RawMessage.
func Marshal(v any) ([]byte, error) {
	var e encoder
	if err := e.encode(reflect.ValueOf(v), 0); err != nil {
		return nil, err
	}
	return e.buf.Bytes(), nil
}

type encoder struct {
	buf bytes.Buffer
}

func (e *encoder) writeString(s []byte) {
	e.buf.WriteString(strconv.Itoa(len(s)))
	e.buf.WriteByte(':')
	e.buf.Write(s)
}

func (e *encoder) encode(v reflect.Value, depth int) error {
	if depth > MaxDepth*4 {
		return &TypeError{Msg: "value nested too deeply (cycle?)"}
	}
	if !v.IsValid() {
		return &TypeError{Msg: "cannot encode nil"}
	}
	if v.Type() == rawType {
		if v.Len() == 0 {
			return &TypeError{Msg: "cannot encode an empty RawMessage"}
		}
		e.buf.Write(v.Bytes())
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return &TypeError{Msg: "cannot encode nil " + v.Type().String()}
		}
		return e.encode(v.Elem(), depth+1)
	case reflect.String:
		e.writeString([]byte(v.String()))
	case reflect.Bool:
		if v.Bool() {
			e.buf.WriteString("i1e")
		} else {
			e.buf.WriteString("i0e")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf.WriteByte('i')
		e.buf.WriteString(strconv.FormatInt(v.Int(), 10))
		e.buf.WriteByte('e')
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		e.buf.WriteByte('i')
		e.buf.WriteString(strconv.FormatUint(v.Uint(), 10))
		e.buf.WriteByte('e')
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if v.Kind() == reflect.Slice {
				e.writeString(v.Bytes())
			} else {
				b := make([]byte, v.Len())
				reflect.Copy(reflect.ValueOf(b), v)
				e.writeString(b)
			}
			return nil
		}
		e.buf.WriteByte('l')
		for i := 0; i < v.Len(); i++ {
			if err := e.encode(v.Index(i), depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte('e')
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return &TypeError{Msg: "map key type must be string, not " + v.Type().Key().String()}
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(a, b int) bool { return keys[a].String() < keys[b].String() })
		e.buf.WriteByte('d')
		for _, k := range keys {
			e.writeString([]byte(k.String()))
			if err := e.encode(v.MapIndex(k), depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte('e')
	case reflect.Struct:
		e.buf.WriteByte('d')
		for _, f := range fieldsOf(v.Type()).sorted {
			fv := v.Field(f.index)
			if isNilish(fv) || (f.omitEmpty && isEmpty(fv)) {
				continue
			}
			e.writeString([]byte(f.key))
			if err := e.encode(fv, depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte('e')
	default:
		return &TypeError{Msg: "unsupported type " + v.Type().String()}
	}
	return nil
}

// isNilish reports values that have no bencode representation and are
// therefore always left out of a struct: nil pointers, nil interfaces and
// nil/empty RawMessages.
func isNilish(v reflect.Value) bool {
	if v.Type() == rawType {
		return v.Len() == 0
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// isEmpty reports whether v is the zero value for omitempty purposes.
func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return false
}
