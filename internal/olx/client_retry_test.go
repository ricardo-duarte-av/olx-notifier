package olx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastClient is a Client with the pacing and backoff delays shrunk so the
// retry tests stay offline and quick.
func fastClient() *Client {
	c := NewClient()
	c.minGap = 5 * time.Millisecond
	c.backoffBase = 5 * time.Millisecond
	return c
}

// TestRetriesThrottled checks a 403 is retried and eventually succeeds.
func TestRetriesThrottled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"data":[{"id":1}]}`))
	}))
	defer srv.Close()

	c := fastClient()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	out, err := c.get(ctx, srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("data = %d, want 1", len(out.Data))
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
}

// TestGivesUpAfterMaxAttempts checks a permanently throttled endpoint stops.
func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := fastClient().get(ctx, srv.URL); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&hits); got != maxAttempts {
		t.Fatalf("hits = %d, want %d", got, maxAttempts)
	}
}

// TestNoRetryOnClientError checks a 404 fails fast instead of burning retries.
func TestNoRetryOnClientError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fastClient().get(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

// TestPaceSpacesRequests checks consecutive requests are spaced apart.
func TestPaceSpacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	c := fastClient()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.get(context.Background(), srv.URL); err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 2*c.minGap {
		t.Fatalf("3 requests took %v, want >= %v", elapsed, 2*c.minGap)
	}
}
