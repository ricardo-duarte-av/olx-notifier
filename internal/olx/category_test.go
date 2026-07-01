package olx

import (
	"context"
	"testing"
	"time"
)

const fakePage = `<html><script>window.__PRERENDERED_STATE__ = "{\"categories\":{\"list\":{\"5407\":{\"id\":5407,\"name\":\"iPhone\",\"label\":\"iphone\",\"parentId\":219,\"path\":\"telemoveis-e-tablets/telemoveis/iphone\",\"level\":3},\"219\":{\"id\":219,\"name\":\"Telemóveis\",\"label\":\"telemoveis\",\"parentId\":25,\"path\":\"telemoveis-e-tablets/telemoveis\",\"level\":2}}}}";</script></html>`

func TestParseCategories(t *testing.T) {
	cats, err := parseCategories([]byte(fakePage))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(cats))
	}
	// sorted by id: 219 then 5407
	if cats[0].ID != 219 || cats[1].ID != 5407 {
		t.Errorf("unexpected order: %d, %d", cats[0].ID, cats[1].ID)
	}
	if cats[0].Name != "Telemóveis" { // ó decoded
		t.Errorf("entity decode failed: %q", cats[0].Name)
	}
	if cats[1].Path != "telemoveis-e-tablets/telemoveis/iphone" {
		t.Errorf("bad path: %q", cats[1].Path)
	}
}

func TestSearchCategories(t *testing.T) {
	cats := []Category{
		{ID: 5407, Name: "iPhone", Path: "telemoveis-e-tablets/telemoveis/iphone", Level: 3},
		{ID: 219, Name: "Telemóveis", Path: "telemoveis-e-tablets/telemoveis", Level: 2},
		{ID: 111, Name: "Capas para iPhone", Path: "acessorios/capas-iphone", Level: 2},
	}
	got := SearchCategories(cats, "iphone", 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 matches, got %d: %+v", len(got), got)
	}
	// exact name match "iPhone" ranks before "Capas para iPhone"
	if got[0].ID != 5407 {
		t.Errorf("expected iPhone (5407) first, got %d", got[0].ID)
	}

	if n := len(SearchCategories(cats, "nomatch", 10)); n != 0 {
		t.Errorf("expected no matches, got %d", n)
	}
	if n := len(SearchCategories(cats, "", 10)); n != 0 {
		t.Errorf("empty term should return nothing, got %d", n)
	}
}

func TestFetchCategoriesLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live network test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cats, err := NewClient().FetchCategories(ctx)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(cats) < 500 {
		t.Fatalf("expected a large category tree, got %d", len(cats))
	}
	iphone := SearchCategories(cats, "iphone", 5)
	if len(iphone) == 0 || iphone[0].ID != 5407 {
		t.Errorf("expected iPhone=5407 top match, got %+v", iphone)
	}
	t.Logf("fetched %d categories", len(cats))
}
