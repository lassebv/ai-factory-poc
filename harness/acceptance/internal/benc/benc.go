// Package benc is the harness's own minimal bencode codec. It exists so that
// test infrastructure (torrent generator, fake tracker, fake peer) never
// depends on torrentd's code. Dicts are map[string]any; strings are string.
package benc

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Encode writes v canonically. Supported: int, int64, string, []byte,
// []any, []string, map[string]any.
func Encode(v any) []byte {
	var b bytes.Buffer
	enc(&b, v)
	return b.Bytes()
}

func enc(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int:
		fmt.Fprintf(b, "i%de", x)
	case int64:
		fmt.Fprintf(b, "i%de", x)
	case string:
		fmt.Fprintf(b, "%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(b, "%d:", len(x))
		b.Write(x)
	case []string:
		b.WriteByte('l')
		for _, s := range x {
			enc(b, s)
		}
		b.WriteByte('e')
	case []any:
		b.WriteByte('l')
		for _, e := range x {
			enc(b, e)
		}
		b.WriteByte('e')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('d')
		for _, k := range keys {
			enc(b, k)
			enc(b, x[k])
		}
		b.WriteByte('e')
	default:
		panic(fmt.Sprintf("benc: unsupported %T", v))
	}
}

// Decode parses one value. It also returns the raw bytes of the top-level
// "info" dict when present (for info-hash computation).
func Decode(data []byte) (v any, info []byte, err error) {
	d := &dec{b: data}
	v, err = d.value(0)
	if err == nil && d.i != len(data) {
		err = errors.New("benc: trailing data")
	}
	return v, d.info, err
}

type dec struct {
	b    []byte
	i    int
	info []byte
}

func (d *dec) value(depth int) (any, error) {
	if d.i >= len(d.b) || depth > 100 {
		return nil, errors.New("benc: truncated or too deep")
	}
	switch c := d.b[d.i]; {
	case c == 'i':
		j := bytes.IndexByte(d.b[d.i:], 'e')
		if j < 0 {
			return nil, errors.New("benc: bad int")
		}
		n, err := strconv.ParseInt(string(d.b[d.i+1:d.i+j]), 10, 64)
		d.i += j + 1
		return n, err
	case c >= '0' && c <= '9':
		j := bytes.IndexByte(d.b[d.i:], ':')
		if j < 0 {
			return nil, errors.New("benc: bad string")
		}
		n, err := strconv.Atoi(string(d.b[d.i : d.i+j]))
		if err != nil || n < 0 || d.i+j+1+n > len(d.b) {
			return nil, errors.New("benc: bad string length")
		}
		s := string(d.b[d.i+j+1 : d.i+j+1+n])
		d.i += j + 1 + n
		return s, nil
	case c == 'l':
		d.i++
		var out []any
		for d.i < len(d.b) && d.b[d.i] != 'e' {
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		d.i++
		return out, nil
	case c == 'd':
		d.i++
		out := map[string]any{}
		for d.i < len(d.b) && d.b[d.i] != 'e' {
			k, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			ks, ok := k.(string)
			if !ok {
				return nil, errors.New("benc: non-string key")
			}
			start := d.i
			v, err := d.value(depth + 1)
			if err != nil {
				return nil, err
			}
			if depth == 0 && ks == "info" {
				d.info = d.b[start:d.i]
			}
			out[ks] = v
		}
		d.i++
		return out, nil
	}
	return nil, fmt.Errorf("benc: unexpected byte %q", d.b[d.i])
}
