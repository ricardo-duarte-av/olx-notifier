package olx

import (
	"strings"
	"testing"
	"time"
)

func TestEdgeBlockedDetection(t *testing.T) {
	tests := []struct {
		name   string
		err    StatusError
		wanted bool
	}{
		{"cloudfront server", StatusError{Code: 403, Server: "CloudFront"}, true},
		{"cloudfront x-cache", StatusError{Code: 403, XCache: "Error from cloudfront"}, true},
		{"origin nginx", StatusError{Code: 403, Server: "nginx", XCache: "Miss from cloudfront"}, false},
		{"no headers", StatusError{Code: 500}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.EdgeBlocked(); got != tt.wanted {
				t.Fatalf("EdgeBlocked() = %v, want %v", got, tt.wanted)
			}
			// A hint is offered only for edge blocks.
			if hasHint := tt.err.Hint() != ""; hasHint != tt.wanted {
				t.Fatalf("Hint() present = %v, want %v", hasHint, tt.wanted)
			}
		})
	}
}

func TestStatusErrorMessage(t *testing.T) {
	edge := &StatusError{Code: 403, Server: "CloudFront", XCache: "Error from cloudfront",
		CFPop: "LIS50-P2", CFID: "abc==", Reason: "Request blocked."}
	msg := edge.Error()
	for _, want := range []string{"HTTP 403", "CloudFront", "Request blocked.", "cf-pop=LIS50-P2", "cf-id=abc=="} {
		if !strings.Contains(msg, want) {
			t.Errorf("edge message missing %q:\n%s", want, msg)
		}
	}

	origin := &StatusError{Code: 429, Server: "nginx", retryAfter: 42 * time.Second}
	msg = origin.Error()
	if !strings.Contains(msg, "origin") || !strings.Contains(msg, "retry-after=42s") {
		t.Errorf("origin message wrong:\n%s", msg)
	}
	if strings.Contains(msg, "CloudFront") {
		t.Errorf("origin message should not blame CloudFront:\n%s", msg)
	}
}

// The reason line is what tells an IP/geo block apart from a generic one.
func TestParseCFReason(t *testing.T) {
	body := []byte(`<H1>403 ERROR</H1>
<H2>The request could not be satisfied.</H2>
<HR noshade size="1px">
Request blocked.
We can't connect to the server for this app or website at this time.`)
	if got := parseCFReason(body); got != "Request blocked." {
		t.Fatalf("parseCFReason() = %q", got)
	}
	if got := parseCFReason([]byte("{}")); got != "" {
		t.Fatalf("parseCFReason(json) = %q, want empty", got)
	}
}

func TestGodebugValue(t *testing.T) {
	v, ok := godebugValue("http2client=0,tlssha1=1", "tlssha1")
	if !ok || v != "1" {
		t.Fatalf("got %q, %v", v, ok)
	}
	if _, ok := godebugValue("http2client=0", "tlssha1"); ok {
		t.Fatal("expected miss")
	}
}

// The daemon and this test binary both carry //go:debug tlssha1=1; if that ever
// regresses, every OLX request 403s, so guard it here.
func TestTLSSHA1WorkaroundActive(t *testing.T) {
	if !tlsSHA1Enabled() {
		t.Fatal("tlssha1=1 not in effect; OLX will return HTTP 403 for every request")
	}
}
