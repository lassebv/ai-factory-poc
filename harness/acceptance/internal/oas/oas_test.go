package oas

import (
	"path/filepath"
	"runtime"
	"testing"
)

func repoSpec(t *testing.T) *Spec {
	_, file, _, _ := runtime.Caller(0)
	s, err := Load(filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "spec", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContractRefsResolve(t *testing.T) {
	if bad := repoSpec(t).Refs(); len(bad) > 0 {
		t.Fatalf("unresolved $refs:\n%v", bad)
	}
}

func TestValidate(t *testing.T) {
	s := repoSpec(t)
	ok := `{"id":"08ada5a7a6183aae1e09d831df6748d566095a10","name":"Sintel","private":false,"sizeBytes":10,
	        "pieceLength":16384,"pieceCount":1,"files":[{"idx":0,"path":"Sintel/a","sizeBytes":10}],
	        "trackers":[{"url":"udp://x:1","tier":0}]}`
	if err := s.ValidateJSON("TorrentPreview", []byte(ok)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"extra field":  `{"id":"08ada5a7a6183aae1e09d831df6748d566095a10","name":"S","private":false,"sizeBytes":1,"pieceLength":1,"pieceCount":1,"files":[],"trackers":[],"extra":1}`,
		"missing name": `{"id":"08ada5a7a6183aae1e09d831df6748d566095a10","private":false,"sizeBytes":1,"pieceLength":1,"pieceCount":1,"files":[],"trackers":[]}`,
		"bad id":       `{"id":"XYZ","name":"S","private":false,"sizeBytes":1,"pieceLength":1,"pieceCount":1,"files":[],"trackers":[]}`,
		"float size":   `{"id":"08ada5a7a6183aae1e09d831df6748d566095a10","name":"S","private":false,"sizeBytes":1.5,"pieceLength":1,"pieceCount":1,"files":[],"trackers":[]}`,
		"bad file":     `{"id":"08ada5a7a6183aae1e09d831df6748d566095a10","name":"S","private":false,"sizeBytes":1,"pieceLength":1,"pieceCount":1,"files":[{"idx":0}],"trackers":[]}`,
	}
	for name, doc := range bad {
		if err := s.ValidateJSON("TorrentPreview", []byte(doc)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	if err := s.ValidateJSON("Error", []byte(`{"error":{"code":"not_found","message":"no such route"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateJSON("Error", []byte(`{"error":{"code":"nope","message":"x"}}`)); err == nil {
		t.Error("unknown error code must fail")
	}
}
