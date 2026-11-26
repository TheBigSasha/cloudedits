package muxaudit

import (
	"net/http"
	"reflect"
	"sort"
	"testing"
)

func TestPatternsEnumeratesEveryRegistration(t *testing.T) {
	noop := func(http.ResponseWriter, *http.Request) {}
	mux := http.NewServeMux()
	want := []string{
		"/",
		"DELETE /assets/{id}/variants/{variantId}",
		"GET /assets/{id}",
		"GET /assets/{id}/file",
		"GET /{$}",
		"POST /ops",
		"example.com/host-scoped",
		"/tree/",
	}
	for _, p := range want {
		mux.HandleFunc(p, noop)
	}
	got := Patterns(mux)
	wantSorted := append([]string{}, want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Fatalf("Patterns = %q\nwant      %q", got, wantSorted)
	}
	if n := len(Patterns(http.NewServeMux())); n != 0 {
		t.Fatalf("empty mux reported %d patterns", n)
	}
}
