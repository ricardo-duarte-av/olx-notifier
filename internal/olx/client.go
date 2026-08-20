package olx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	baseURL   = "https://www.olx.pt/api/v1/offers/"
	pageLimit = 50 // API hard cap
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0"

	// minRequestGap is the smallest spacing between two OLX requests. Polling
	// every search back-to-back (each up to maxPages requests) reads as a burst
	// and earns an HTTP 403, so all requests funnel through one pacer.
	minRequestGap = 700 * time.Millisecond

	// maxAttempts counts the first try plus retries for a throttled request.
	maxAttempts = 4

	// backoffBase is the first retry delay; it doubles per attempt.
	backoffBase = time.Second
)

// StatusError is returned when OLX answers with a non-200 status. It carries
// enough edge diagnostics to tell an IP/WAF block apart from a genuine API
// error, because both surface as a bare HTTP 403.
type StatusError struct {
	Code int
	URL  string

	// Edge diagnostics, from the response headers.
	Server string // "CloudFront" when the edge answered, "nginx" when OLX did
	XCache string // e.g. "Error from cloudfront" / "Miss from cloudfront"
	CFPop  string // edge location that served the block, e.g. "LIS50-P2"
	CFID   string // X-Amz-Cf-Id, the reference to quote to OLX/AWS support

	// Reason is the human-readable line lifted from the CloudFront error page,
	// e.g. "Request blocked." or a geo-restriction message.
	Reason string

	retryAfter time.Duration // from the Retry-After header, if any
}

// EdgeBlocked reports whether CloudFront rejected the request itself, so it
// never reached OLX. Those are the blocks driven by source IP reputation, rate
// rules, geo restrictions or the client's TLS fingerprint.
func (e *StatusError) EdgeBlocked() bool {
	return strings.EqualFold(e.Server, "CloudFront") || strings.Contains(strings.ToLower(e.XCache), "error from cloudfront")
}

func (e *StatusError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "olx returned HTTP %d", e.Code)

	if e.EdgeBlocked() {
		b.WriteString(": blocked by CloudFront at the edge")
		if e.Reason != "" {
			fmt.Fprintf(&b, " (%q)", e.Reason)
		}
		b.WriteString(" — the request never reached OLX, so it is not about this search's query or filters")
	} else if e.Server != "" {
		fmt.Fprintf(&b, " from origin (server=%s)", e.Server)
	}

	var meta []string
	if e.CFPop != "" {
		meta = append(meta, "cf-pop="+e.CFPop)
	}
	if e.CFID != "" {
		meta = append(meta, "cf-id="+e.CFID)
	}
	if e.retryAfter > 0 {
		meta = append(meta, "retry-after="+e.retryAfter.String())
	}
	if len(meta) > 0 {
		fmt.Fprintf(&b, " [%s]", strings.Join(meta, " "))
	}
	return b.String()
}

// Hint returns a one-line operator hint for an edge block, or "" when the error
// needs no explaining. It names the likely causes in the order worth checking.
func (e *StatusError) Hint() string {
	if !e.EdgeBlocked() {
		return ""
	}
	tls := "the //go:debug tlssha1=1 TLS-fingerprint workaround is ACTIVE"
	if !tlsSHA1Enabled() {
		tls = "the //go:debug tlssha1=1 TLS-fingerprint workaround is NOT active — that alone causes a permanent 403"
	}
	return "likely cause: this host's source IP (reputation, rate limits or geo), or the client TLS fingerprint; " + tls +
		". Check whether the same request succeeds from another network, e.g. curl from a different IP"
}

// tlsSHA1Enabled reports whether tlssha1=1 is in effect, via the environment or
// the //go:debug directive baked into the binary.
func tlsSHA1Enabled() bool {
	if v, ok := godebugValue(os.Getenv("GODEBUG"), "tlssha1"); ok {
		return v == "1"
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "DefaultGODEBUG" {
				if v, ok := godebugValue(s.Value, "tlssha1"); ok {
					return v == "1"
				}
			}
		}
	}
	return false
}

// godebugValue pulls one setting out of a comma-separated GODEBUG string.
func godebugValue(godebug, key string) (string, bool) {
	for _, kv := range strings.Split(godebug, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

// cfReasonRe pulls the explanatory sentence out of a CloudFront error page.
var cfReasonRe = regexp.MustCompile(`(?i)<HR noshade size="1px">\s*([^<\n]+)`)

// parseCFReason extracts the human-readable reason from a CloudFront error body.
func parseCFReason(body []byte) string {
	m := cfReasonRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(string(m[1]))
}

// Throttled reports whether the status looks like rate limiting or bot
// detection rather than a permanent problem with the request itself.
func (e *StatusError) Throttled() bool {
	return e.Code == http.StatusForbidden || e.Code == http.StatusTooManyRequests || e.Code >= 500
}

// Client talks to the OLX offers API.
type Client struct {
	http *http.Client

	// Tunables, overridden in tests to keep them fast.
	minGap      time.Duration
	backoffBase time.Duration

	mu   sync.Mutex
	last time.Time // when the previous request was issued
}

// NewClient returns a Client with a sane default timeout.
func NewClient() *Client {
	return &Client{
		http:        &http.Client{Timeout: 30 * time.Second},
		minGap:      minRequestGap,
		backoffBase: backoffBase,
	}
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
			// Pages already collected are still usable; a mid-pagination
			// failure shouldn't discard them and re-notify later. Say so
			// though, so a partial result is never mistaken for a full one.
			if len(out) > 0 {
				log.Printf("olx: page %d of %q failed, continuing with %d offer(s) from %d page(s): %v",
					page+1, p.Query, len(out), page, err)
				return out, nil
			}
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

// pace blocks until at least minRequestGap has passed since the previous
// request, so concurrent callers (poller plus chat commands) share one budget.
func (c *Client) pace(ctx context.Context) error {
	c.mu.Lock()
	wait := time.Until(c.last.Add(c.minGap))
	if wait < 0 {
		wait = 0
	}
	c.last = time.Now().Add(wait)
	c.mu.Unlock()

	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) get(ctx context.Context, reqURL string) (*apiResponse, error) {
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, c.backoff(attempt, lastErr)); err != nil {
				return nil, err
			}
		}
		if err := c.pace(ctx); err != nil {
			return nil, err
		}

		out, err := c.doGet(ctx, reqURL)
		if err == nil {
			// Only worth a line if it took retries to get here; a transient
			// block that clears on its own is still useful to know about.
			if attempt > 0 {
				log.Printf("olx: recovered after %d attempt(s); previous error: %v", attempt+1, lastErr)
			}
			return out, nil
		}
		lastErr = err

		var se *StatusError
		if !errors.As(err, &se) || !se.Throttled() {
			return nil, err
		}
	}

	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

func (c *Client) doGet(ctx context.Context, reqURL string) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("olx request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read a slice of the error page so the CloudFront reason can be
		// reported; this also lets the connection be reused.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &StatusError{
			Code:       resp.StatusCode,
			URL:        reqURL,
			Server:     resp.Header.Get("Server"),
			XCache:     resp.Header.Get("X-Cache"),
			CFPop:      resp.Header.Get("X-Amz-Cf-Pop"),
			CFID:       resp.Header.Get("X-Amz-Cf-Id"),
			Reason:     parseCFReason(body),
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	var out apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode olx response: %w", err)
	}
	return &out, nil
}

// setBrowserHeaders makes the request look like the site's own XHR. OLX's edge
// rejects requests that carry only a User-Agent.
func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "pt-PT,pt;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://www.olx.pt/")
	req.Header.Set("Origin", "https://www.olx.pt")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Connection", "keep-alive")
}

// backoff returns how long to wait before the given attempt (1-based retry
// number), honouring Retry-After when OLX sent one.
func (c *Client) backoff(attempt int, err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) && se.retryAfter > 0 {
		return se.retryAfter
	}
	d := time.Duration(1<<uint(attempt-1)) * c.backoffBase // 1x, 2x, 4x
	return d + time.Duration(rand.Int63n(int64(d/4+1)))
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
