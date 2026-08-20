// Live tests need the same TLS fingerprint workaround as the daemon; see the
// //go:debug note in main.go.
//
//go:debug tlssha1=1
package olx

import (
	"context"
	"testing"
	"time"
)

// TestLiveSearch hits the real OLX API. Run explicitly with:
//
//	go test ./internal/olx -run TestLiveSearch -v
//
// Skipped in -short mode so normal test runs stay offline.
func TestLiveSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live network test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	min, max := 100, 200
	cat := 5407 // iphone
	offers, err := NewClient().Search(ctx, SearchParams{
		Query:      "iphone",
		MinPrice:   &min,
		MaxPrice:   &max,
		CategoryID: &cat,
	}, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(offers) == 0 {
		t.Fatal("expected some offers, got none")
	}

	seen := map[int64]bool{}
	priced := 0
	for _, o := range offers {
		if seen[o.ID] {
			t.Errorf("duplicate offer id %d not deduped", o.ID)
		}
		seen[o.ID] = true
		if p, ok := o.Price(); ok {
			priced++
			if p < min || p > max {
				t.Errorf("offer %d price %d outside filter [%d,%d]", o.ID, p, min, max)
			}
		}
	}
	if priced == 0 {
		t.Error("no offers had a parseable price")
	}
	t.Logf("fetched %d offers, %d with price", len(offers), priced)
}
