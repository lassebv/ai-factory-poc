package torrentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestWebFSHasIndex(t *testing.T) {
	b, err := fs.ReadFile(WebFS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<title>torrentd") {
		t.Fatalf("index.html has no torrentd title: %.200s", b)
	}
}
