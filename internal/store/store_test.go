package store

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
)

func offer(id int64, price int) olx.Offer {
	return olx.Offer{
		ID:    id,
		Title: "Ad",
		URL:   "https://olx.pt/x",
		Params: []olx.Param{{
			Key:   "price",
			Value: olx.ParamValue{Value: json.Number(itoa(price)), Label: itoa(price) + " €"},
		}},
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// reload fetches the search fresh so callers see the updated seeded flag.
func reload(t *testing.T, s *Store, id int64) Search {
	t.Helper()
	list, err := s.ListSearches()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, se := range list {
		if se.ID == id {
			return se
		}
	}
	t.Fatalf("search %d not found", id)
	return Search{}
}

func TestReconcileSeedThenDiff(t *testing.T) {
	s := openTemp(t)
	min := 100
	id, err := s.AddSearch(olx.SearchParams{Query: "iphone", MinPrice: &min})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	// First reconcile seeds silently: no events even with two ads.
	ev, err := s.Reconcile(reload(t, s, id), []olx.Offer{offer(1, 100), offer(2, 200)})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("seeding should emit no events, got %d", len(ev))
	}

	// Unchanged ad 1, new ad 3, price change on ad 2.
	ev, err = s.Reconcile(reload(t, s, id),
		[]olx.Offer{offer(1, 100), offer(2, 150), offer(3, 300)})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(ev) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(ev), ev)
	}

	var gotNew, gotChange bool
	for _, e := range ev {
		switch e.Type {
		case EventNew:
			gotNew = true
			if e.Offer.ID != 3 {
				t.Errorf("new event for wrong ad %d", e.Offer.ID)
			}
		case EventPriceChange:
			gotChange = true
			if e.Offer.ID != 2 {
				t.Errorf("price change for wrong ad %d", e.Offer.ID)
			}
			if e.OldPrice == nil || *e.OldPrice != 200 {
				t.Errorf("expected old price 200, got %v", e.OldPrice)
			}
			if p, _ := e.Offer.Price(); p != 150 {
				t.Errorf("expected new price 150, got %d", p)
			}
		}
	}
	if !gotNew || !gotChange {
		t.Errorf("missing event: new=%v change=%v", gotNew, gotChange)
	}

	// A third reconcile with no changes emits nothing.
	ev, err = s.Reconcile(reload(t, s, id),
		[]olx.Offer{offer(1, 100), offer(2, 150), offer(3, 300)})
	if err != nil {
		t.Fatalf("noop: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("expected no events, got %d", len(ev))
	}

	n, err := s.AdCount(id)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 stored ads, got %d", n)
	}
}

func TestRemoveSearchCascades(t *testing.T) {
	s := openTemp(t)
	id, _ := s.AddSearch(olx.SearchParams{Query: "x"})
	if _, err := s.Reconcile(reload(t, s, id), []olx.Offer{offer(1, 10)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	removed, err := s.RemoveSearch(id)
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	// seen_ads for that search must be gone (FK cascade).
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM seen_ads WHERE search_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected cascade delete, %d rows remain", n)
	}
}
