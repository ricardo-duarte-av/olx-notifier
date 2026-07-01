package olx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	baseURL   = "https://www.olx.pt/api/v1/offers/"
	pageLimit = 50 // API hard cap
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0"
)

// Client talks to the OLX offers API.
type Client struct {
	http *http.Client
}

// NewClient returns a Client with a sane default timeout.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second}}
}

// Search fetches all offers matching params, paginating up to maxPages and
// deduplicating by offer ID (promoted ads repeat across pages). Results come
// back newest-first.
func (c *Client) Search(ctx context.Context, p SearchParams, maxPages int) ([]Offer, error) {
	if maxPages < 1 {
		maxPages = 1
	}

	seen := make(map[int64]struct{})
	var out []Offer

	for page := 0; page < maxPages; page++ {
		reqURL := buildURL(p, page*pageLimit)
		resp, err := c.get(ctx, reqURL)
		if err != nil {
			return nil, err
		}

		for _, o := range resp.Data {
			if _, dup := seen[o.ID]; dup {
				continue
			}
			seen[o.ID] = struct{}{}
			out = append(out, o)
		}

		// Stop when the API says there's no further page, or it returned a
		// short page (fewer than the requested limit of organic results).
		if resp.Links.Next.Href == "" || len(resp.Data) == 0 {
			break
		}
	}

	return out, nil
}

func (c *Client) get(ctx context.Context, reqURL string) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("olx request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("olx returned HTTP %d", resp.StatusCode)
	}

	var out apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode olx response: %w", err)
	}
	return &out, nil
}

// buildURL constructs an offers API URL for the given params and offset.
func buildURL(p SearchParams, offset int) string {
	q := url.Values{}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(pageLimit))
	q.Set("search[order]", "created_at:desc")
	if p.Query != "" {
		q.Set("query", p.Query)
	}
	if p.MinPrice != nil {
		q.Set("filter_float_price:from", strconv.Itoa(*p.MinPrice))
	}
	if p.MaxPrice != nil {
		q.Set("filter_float_price:to", strconv.Itoa(*p.MaxPrice))
	}
	if p.CategoryID != nil {
		q.Set("category_id", strconv.Itoa(*p.CategoryID))
	}
	return baseURL + "?" + q.Encode()
}
