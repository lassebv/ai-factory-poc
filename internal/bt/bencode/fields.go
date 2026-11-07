package bencode

import (
	"reflect"
	"sort"
	"strings"
	"sync"
)

// field is one encodable struct field.
type field struct {
	key       string
	index     int
	omitEmpty bool
}

// structInfo is the cached field layout of a struct type.
type structInfo struct {
	sorted []field          // in key order, for encoding
	byKey  map[string]field // for decoding
}

var fieldCache sync.Map // reflect.Type → *structInfo

// fieldsOf returns the cached field layout of struct type t.
func fieldsOf(t reflect.Type) *structInfo {
	if v, ok := fieldCache.Load(t); ok {
		return v.(*structInfo)
	}
	info := &structInfo{byKey: map[string]field{}}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("bencode")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = sf.Name
		}
		f := field{key: name, index: i, omitEmpty: hasOpt(opts, "omitempty")}
		if _, dup := info.byKey[name]; dup {
			continue // first field with a key wins
		}
		info.byKey[name] = f
		info.sorted = append(info.sorted, f)
	}
	sort.Slice(info.sorted, func(a, b int) bool { return info.sorted[a].key < info.sorted[b].key })
	v, _ := fieldCache.LoadOrStore(t, info)
	return v.(*structInfo)
}

func hasOpt(opts, want string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == want {
			return true
		}
	}
	return false
}
