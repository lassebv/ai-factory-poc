package bencode

import (
	"bytes"
	"reflect"
	"strconv"
)

// Unmarshal decodes in lenient mode: accepts unsorted (and duplicate) dict
// keys and non-canonical integers and lengths. Rejects malformed input and
// trailing data after the value.
func Unmarshal(data []byte, v any) error { return unmarshal(data, v, false) }

// UnmarshalStrict decodes in strict mode (A1): additionally rejects
// non-canonical integers (leading zeros, "-0", empty), non-canonical string
// lengths (leading zeros), unsorted dict keys and duplicate dict keys.
func UnmarshalStrict(data []byte, v any) error { return unmarshal(data, v, true) }

// Valid reports whether data is exactly one well-formed bencode value. With
// strict it also requires the canonical encoding.
func Valid(data []byte, strict bool) bool {
	d := decoder{data: data, strict: strict}
	return d.top(reflect.Value{}) == nil
}

func unmarshal(data []byte, v any, strict bool) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrNilTarget
	}
	d := decoder{data: data, strict: strict}
	return d.top(rv.Elem())
}

type decoder struct {
	data   []byte
	pos    int
	strict bool
}

func (d *decoder) syntax(msg string) error { return &SyntaxError{Offset: d.pos, Msg: msg} }

// top decodes exactly one value into v (invalid v means validate only).
func (d *decoder) top(v reflect.Value) error {
	if err := d.value(v, 0); err != nil {
		return err
	}
	if d.pos != len(d.data) {
		return d.syntax("trailing data after value")
	}
	return nil
}

func (d *decoder) peek() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, d.syntax("unexpected end of input")
	}
	return d.data[d.pos], nil
}

// value decodes the next value into v. An invalid v skips (but fully
// validates) the value.
func (d *decoder) value(v reflect.Value, depth int) error {
	if v.IsValid() {
		if v.Type() == rawType {
			start := d.pos
			if err := d.value(reflect.Value{}, depth); err != nil {
				return err
			}
			v.SetBytes(bytes.Clone(d.data[start:d.pos]))
			return nil
		}
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			return d.value(v.Elem(), depth)
		case reflect.Interface:
			if v.NumMethod() != 0 {
				return &TypeError{Msg: "cannot decode into non-empty interface " + v.Type().String()}
			}
			g, err := d.generic(depth)
			if err != nil {
				return err
			}
			v.Set(reflect.ValueOf(g))
			return nil
		}
	}
	c, err := d.peek()
	if err != nil {
		return err
	}
	switch {
	case c == 'i':
		return d.intValue(v)
	case c >= '0' && c <= '9':
		s, err := d.str()
		if err != nil {
			return err
		}
		return setString(v, s)
	case c == 'l':
		return d.list(v, depth+1)
	case c == 'd':
		return d.dict(v, depth+1)
	}
	return d.syntax("invalid value prefix " + strconv.QuoteRune(rune(c)))
}

// intText consumes "i…e" and returns the validated digits (with sign).
func (d *decoder) intText() (string, error) {
	start := d.pos + 1
	end := bytes.IndexByte(d.data[start:], 'e')
	if end < 0 {
		return "", d.syntax("unterminated integer")
	}
	txt := d.data[start : start+end]
	digits := txt
	neg := len(txt) > 0 && txt[0] == '-'
	if neg {
		digits = txt[1:]
	}
	if len(digits) == 0 {
		return "", d.syntax("empty integer")
	}
	for _, b := range digits {
		if b < '0' || b > '9' {
			return "", d.syntax("invalid integer")
		}
	}
	if d.strict {
		if digits[0] == '0' && (len(digits) > 1 || neg) {
			return "", d.syntax("non-canonical integer")
		}
	}
	d.pos = start + end + 1
	return string(txt), nil
}

func (d *decoder) intValue(v reflect.Value) error {
	txt, err := d.intText()
	if err != nil {
		return err
	}
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(txt, 10, 64)
		if err != nil || v.OverflowInt(n) {
			return &TypeError{Msg: "integer " + txt + " overflows " + v.Type().String()}
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if txt == "-0" {
			txt = "0"
		}
		n, err := strconv.ParseUint(txt, 10, 64)
		if err != nil || v.OverflowUint(n) {
			return &TypeError{Msg: "integer " + txt + " does not fit " + v.Type().String()}
		}
		v.SetUint(n)
	case reflect.Bool:
		n, err := strconv.ParseInt(txt, 10, 64)
		if err != nil {
			return &TypeError{Msg: "integer " + txt + " overflows bool"}
		}
		v.SetBool(n != 0)
	default:
		return &TypeError{Msg: "cannot decode an integer into " + v.Type().String()}
	}
	return nil
}

// str consumes "<len>:<bytes>" and returns the bytes (aliasing the input).
func (d *decoder) str() ([]byte, error) {
	start := d.pos
	n := 0
	i := start
	for ; i < len(d.data) && d.data[i] >= '0' && d.data[i] <= '9'; i++ {
		n = n*10 + int(d.data[i]-'0')
		if n > len(d.data) { // also guards against overflow
			return nil, d.syntax("string length exceeds the input")
		}
	}
	if i >= len(d.data) || d.data[i] != ':' {
		return nil, d.syntax("invalid string length")
	}
	if d.strict && d.data[start] == '0' && i-start > 1 {
		return nil, d.syntax("non-canonical string length")
	}
	i++
	if n > len(d.data)-i {
		return nil, d.syntax("string length exceeds the input")
	}
	d.pos = i + n
	return d.data[i : i+n], nil
}

func setString(v reflect.Value, s []byte) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(string(s))
		return nil
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes(bytes.Clone(s))
			return nil
		}
	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if v.Len() != len(s) {
				return &TypeError{Msg: "string of length " + strconv.Itoa(len(s)) + " does not fit " + v.Type().String()}
			}
			reflect.Copy(v, reflect.ValueOf(s))
			return nil
		}
	}
	return &TypeError{Msg: "cannot decode a string into " + v.Type().String()}
}

func (d *decoder) list(v reflect.Value, depth int) error {
	if depth > MaxDepth {
		return d.syntax("nesting too deep")
	}
	var (
		kind  reflect.Kind
		slice reflect.Value
	)
	if v.IsValid() {
		kind = v.Kind()
		switch {
		case kind == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8:
			slice = reflect.MakeSlice(v.Type(), 0, 0)
		case kind == reflect.Array && v.Type().Elem().Kind() != reflect.Uint8:
		default:
			return &TypeError{Msg: "cannot decode a list into " + v.Type().String()}
		}
	}
	d.pos++ // 'l'
	for n := 0; ; n++ {
		c, err := d.peek()
		if err != nil {
			return err
		}
		if c == 'e' {
			d.pos++
			break
		}
		var elem reflect.Value
		switch {
		case kind == reflect.Slice:
			elem = reflect.New(v.Type().Elem()).Elem()
		case kind == reflect.Array:
			if n >= v.Len() {
				return &TypeError{Msg: "list too long for " + v.Type().String()}
			}
			elem = v.Index(n)
		}
		if err := d.value(elem, depth); err != nil {
			return err
		}
		if kind == reflect.Slice {
			slice = reflect.Append(slice, elem)
		}
	}
	if kind == reflect.Slice {
		v.Set(slice)
	}
	return nil
}

func (d *decoder) dict(v reflect.Value, depth int) error {
	if depth > MaxDepth {
		return d.syntax("nesting too deep")
	}
	var (
		si *structInfo
		m  reflect.Value
	)
	if v.IsValid() {
		switch {
		case v.Kind() == reflect.Struct:
			si = fieldsOf(v.Type())
		case v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String:
			m = reflect.MakeMap(v.Type())
		default:
			return &TypeError{Msg: "cannot decode a dict into " + v.Type().String()}
		}
	}
	d.pos++ // 'd'
	var prev []byte
	for first := true; ; first = false {
		c, err := d.peek()
		if err != nil {
			return err
		}
		if c == 'e' {
			d.pos++
			break
		}
		if c < '0' || c > '9' {
			return d.syntax("dict key is not a string")
		}
		keyPos := d.pos
		key, err := d.str()
		if err != nil {
			return err
		}
		if d.strict && !first {
			switch cmp := bytes.Compare(prev, key); {
			case cmp == 0:
				return &SyntaxError{Offset: keyPos, Msg: "duplicate dict key " + strconv.Quote(string(key))}
			case cmp > 0:
				return &SyntaxError{Offset: keyPos, Msg: "unsorted dict key " + strconv.Quote(string(key))}
			}
		}
		prev = key
		switch {
		case si != nil:
			var fv reflect.Value
			if f, ok := si.byKey[string(key)]; ok {
				fv = v.Field(f.index)
			}
			if err := d.value(fv, depth); err != nil {
				return err
			}
		case m.IsValid():
			elem := reflect.New(v.Type().Elem()).Elem()
			if err := d.value(elem, depth); err != nil {
				return err
			}
			m.SetMapIndex(reflect.ValueOf(string(key)).Convert(v.Type().Key()), elem)
		default:
			if err := d.value(reflect.Value{}, depth); err != nil {
				return err
			}
		}
	}
	if m.IsValid() {
		v.Set(m)
	}
	return nil
}

// generic decodes the next value into the any representation: int64,
// string, []any and map[string]any.
func (d *decoder) generic(depth int) (any, error) {
	c, err := d.peek()
	if err != nil {
		return nil, err
	}
	switch {
	case c == 'i':
		txt, err := d.intText()
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(txt, 10, 64)
		if err != nil {
			return nil, &TypeError{Msg: "integer " + txt + " overflows int64"}
		}
		return n, nil
	case c >= '0' && c <= '9':
		s, err := d.str()
		if err != nil {
			return nil, err
		}
		return string(s), nil
	case c == 'l':
		var out []any
		err := d.list(reflect.ValueOf(&out).Elem(), depth+1)
		if out == nil && err == nil {
			out = []any{}
		}
		return out, err
	case c == 'd':
		var out map[string]any
		err := d.dict(reflect.ValueOf(&out).Elem(), depth+1)
		return out, err
	}
	return nil, d.syntax("invalid value prefix " + strconv.QuoteRune(rune(c)))
}
