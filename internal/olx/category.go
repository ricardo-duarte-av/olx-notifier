package olx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Category is a node in the OLX category tree.
type Category struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`  // display name, e.g. "iPhone"
	Label    string `json:"label"` // slug, e.g. "iphone"
	ParentID int    `json:"parentId"`
	Path     string `json:"path"` // e.g. "telemoveis-e-tablets/telemoveis/iphone"
	Level    int    `json:"level"`
}

// categorySourceURL is a listing page whose embedded state carries the full
// category tree.
const categorySourceURL = "https://www.olx.pt/ads/"

var prerenderedRe = regexp.MustCompile(`(?s)window\.__PRERENDERED_STATE__\s*=\s*"(.*?)";`)

// FetchCategories downloads and parses the OLX category tree from the page's
// embedded __PRERENDERED_STATE__ blob.
func (c *Client) FetchCategories(ctx context.Context) ([]Category, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, categorySourceURL, nil)
	if err != nil {
		return nil, err
	}
	// A document fetch, not the JSON XHR, so it gets page-shaped headers.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pt-PT,pt;q=0.9,en;q=0.8")

	if err := c.pace(ctx); err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch categories: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("categories page HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseCategories(body)
}

func parseCategories(html []byte) ([]Category, error) {
	m := prerenderedRe.FindSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("category state not found on page")
	}
	// The captured group is a JSON-string-escaped JSON document: unquote once to
	// get the inner JSON, then decode it.
	var inner string
	if err := json.Unmarshal([]byte(`"`+string(m[1])+`"`), &inner); err != nil {
		return nil, fmt.Errorf("unquote category state: %w", err)
	}
	var state struct {
		Categories struct {
			List map[string]Category `json:"list"`
		} `json:"categories"`
	}
	if err := json.Unmarshal([]byte(inner), &state); err != nil {
		return nil, fmt.Errorf("decode category state: %w", err)
	}
	if len(state.Categories.List) == 0 {
		return nil, fmt.Errorf("category list empty")
	}

	out := make([]Category, 0, len(state.Categories.List))
	for _, cat := range state.Categories.List {
		out = append(out, cat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// topLevel is the tree depth of the root sections (single-segment paths).
const topLevel = 1

// TopLevelCategories returns the root sections, sorted by name.
func TopLevelCategories(cats []Category) []Category {
	var out []Category
	for _, c := range cats {
		if c.Level == topLevel {
			out = append(out, c)
		}
	}
	sortByName(out)
	return out
}

// ChildCategories returns the direct children of parentID, sorted by name.
func ChildCategories(cats []Category, parentID int) []Category {
	var out []Category
	for _, c := range cats {
		if c.ParentID == parentID {
			out = append(out, c)
		}
	}
	sortByName(out)
	return out
}

// CategoryByID returns the category with the given id, if present.
func CategoryByID(cats []Category, id int) (Category, bool) {
	for _, c := range cats {
		if c.ID == id {
			return c, true
		}
	}
	return Category{}, false
}

func sortByName(cats []Category) {
	sort.SliceStable(cats, func(i, j int) bool { return cats[i].Name < cats[j].Name })
}

// SearchCategories returns categories matching term (case-insensitive), ranked
// by how closely the name matches, then path, capped at limit results.
func SearchCategories(cats []Category, term string, limit int) []Category {
	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return nil
	}

	type scored struct {
		cat   Category
		score int
	}
	var matches []scored
	for _, c := range cats {
		name := strings.ToLower(c.Name)
		path := strings.ToLower(c.Path)
		switch {
		case name == term:
			matches = append(matches, scored{c, 0})
		case strings.HasPrefix(name, term):
			matches = append(matches, scored{c, 1})
		case strings.Contains(name, term):
			matches = append(matches, scored{c, 2})
		case strings.Contains(path, term):
			matches = append(matches, scored{c, 3})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score < matches[j].score
		}
		if matches[i].cat.Level != matches[j].cat.Level {
			return matches[i].cat.Level < matches[j].cat.Level
		}
		return matches[i].cat.Name < matches[j].cat.Name
	})

	out := make([]Category, 0, limit)
	for _, m := range matches {
		if len(out) >= limit {
			break
		}
		out = append(out, m.cat)
	}
	return out
}
