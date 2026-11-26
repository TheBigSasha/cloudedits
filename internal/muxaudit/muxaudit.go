// Package muxaudit enumerates the patterns actually registered on an
// http.ServeMux, so tests can prove that the live router contains exactly the
// declared routes and nothing registered behind the route table's back.
package muxaudit

import (
	"net/http"
	"reflect"
	"sort"
)

// Patterns returns every pattern string registered on mux (for example
// "GET /assets/{id}" or "/"), sorted. It walks the router's internal state by
// reflection rather than trusting any list the caller keeps, so a handler
// added with a stray mux.Handle call is reported too.
func Patterns(mux *http.ServeMux) []string {
	found := map[string]bool{}
	walk(reflect.ValueOf(mux), map[uintptr]bool{}, found)
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func walk(v reflect.Value, seen map[uintptr]bool, found map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return
		}
		seen[v.Pointer()] = true
		walk(v.Elem(), seen, found)
	case reflect.Interface:
		if !v.IsNil() {
			walk(v.Elem(), seen, found)
		}
	case reflect.Struct:
		if t := v.Type(); t.PkgPath() == "net/http" && t.Name() == "pattern" {
			if str := v.FieldByName("str"); str.IsValid() && str.Kind() == reflect.String {
				found[str.String()] = true
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			walk(v.Field(i), seen, found)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walk(v.Index(i), seen, found)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			walk(it.Key(), seen, found)
			walk(it.Value(), seen, found)
		}
	}
}
