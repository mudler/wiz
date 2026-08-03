//go:build integration

package mcp

import (
	"context"
	"testing"
)

// TestSearchWebLive hits the real DuckDuckGo endpoint to catch markup drift.
// Run with: go test -tags integration ./mcp/ -run TestSearchWebLive
func TestSearchWebLive(t *testing.T) {
	_, out, err := (&webServer{classifier: allowClassifier{}}).search(context.Background(), nil, webSearchInput{Query: "golang programming language", MaxResults: 5})
	if err != nil {
		t.Fatalf("searchWeb: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("output error: %s", out.Error)
	}
	if len(out.Results) == 0 {
		t.Fatal("expected at least one live result; DDG markup may have changed")
	}
	for i, r := range out.Results {
		if r.Title == "" || r.URL == "" {
			t.Errorf("result %d missing title/url: %+v", i, r)
		}
	}
}
