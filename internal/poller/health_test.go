package poller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
	"github.com/ricardo-duarte-av/olx-notifier/internal/store"
)

// alertSpy captures room alerts; only Alert matters for these tests.
type alertSpy struct{ alerts []string }

func (*alertSpy) Notify(context.Context, store.Search, []store.Event) {}

func (a *alertSpy) Alert(_ context.Context, text string) { a.alerts = append(a.alerts, text) }

var blocked = &olx.StatusError{Code: 403, Server: "CloudFront", Reason: "Request blocked."}

// A total outage alerts once, stays quiet while it persists, then alerts again
// on recovery.
func TestOutageAlertsOnceThenOnRecovery(t *testing.T) {
	spy := &alertSpy{}
	p := &Poller{notifier: spy}
	ctx := context.Background()

	p.reportHealth(ctx, 3, 3, 3, blocked)
	if len(spy.alerts) != 1 {
		t.Fatalf("first outage cycle: %d alert(s), want 1", len(spy.alerts))
	}
	if !strings.Contains(spy.alerts[0], "all 3 search(es)") || !strings.Contains(spy.alerts[0], "host-wide") {
		t.Errorf("alert text unhelpful:\n%s", spy.alerts[0])
	}

	// Still broken on the next ticks: must not repost.
	for i := 0; i < 5; i++ {
		p.reportHealth(ctx, 3, 3, 3, blocked)
	}
	if len(spy.alerts) != 1 {
		t.Fatalf("ongoing outage: %d alert(s), want 1", len(spy.alerts))
	}

	// Recovery posts exactly one more.
	p.reportHealth(ctx, 3, 0, 0, nil)
	if len(spy.alerts) != 2 {
		t.Fatalf("recovery: %d alert(s), want 2", len(spy.alerts))
	}
	if !strings.Contains(spy.alerts[1], "recovered") {
		t.Errorf("recovery alert wrong:\n%s", spy.alerts[1])
	}

	// A healthy cycle after recovery is silent.
	p.reportHealth(ctx, 3, 0, 0, nil)
	if len(spy.alerts) != 2 {
		t.Fatalf("healthy cycle alerted: %d", len(spy.alerts))
	}
}

// An ongoing outage re-alerts once alertRepeat has elapsed.
func TestOutageRepeatsAfterInterval(t *testing.T) {
	spy := &alertSpy{}
	p := &Poller{notifier: spy}
	p.reportHealth(context.Background(), 2, 2, 2, blocked)

	p.lastAlertAt = time.Now().Add(-alertRepeat - time.Minute)
	p.outageSince = time.Now().Add(-2 * time.Hour)
	p.reportHealth(context.Background(), 2, 2, 2, blocked)

	if len(spy.alerts) != 2 {
		t.Fatalf("got %d alert(s), want 2", len(spy.alerts))
	}
	if !strings.Contains(spy.alerts[1], "still failing") {
		t.Errorf("repeat alert should say it is ongoing:\n%s", spy.alerts[1])
	}
}

// Partial failure is not an outage: no alert, since some searches still work.
func TestPartialFailureDoesNotAlert(t *testing.T) {
	spy := &alertSpy{}
	p := &Poller{notifier: spy}
	p.reportHealth(context.Background(), 3, 2, 2, blocked)
	if len(spy.alerts) != 0 {
		t.Fatalf("partial failure alerted: %v", spy.alerts)
	}
}

// A non-CloudFront total failure still alerts, without blaming the edge.
func TestGenericOutageAlerts(t *testing.T) {
	spy := &alertSpy{}
	p := &Poller{notifier: spy}
	p.reportHealth(context.Background(), 2, 2, 0, errors.New("dial tcp: no route to host"))
	if len(spy.alerts) != 1 {
		t.Fatalf("got %d alert(s), want 1", len(spy.alerts))
	}
	if strings.Contains(spy.alerts[0], "CloudFront") {
		t.Errorf("should not blame CloudFront:\n%s", spy.alerts[0])
	}
	if !strings.Contains(spy.alerts[0], "no route to host") {
		t.Errorf("should carry the detail:\n%s", spy.alerts[0])
	}
}

// Disabling every search during an outage clears the state silently, rather
// than announcing a bogus recovery later.
func TestNoSearchesClearsOutage(t *testing.T) {
	spy := &alertSpy{}
	p := &Poller{notifier: spy}
	ctx := context.Background()

	p.reportHealth(ctx, 2, 2, 2, blocked) // outage begins
	p.reportHealth(ctx, 0, 0, 0, nil)     // all searches disabled
	p.reportHealth(ctx, 1, 0, 0, nil)     // one re-enabled, works

	if len(spy.alerts) != 1 {
		t.Fatalf("got %d alert(s), want 1 (the outage only): %v", len(spy.alerts), spy.alerts)
	}
}

// Jitter stays within bounds and actually varies.
func TestNextInterval(t *testing.T) {
	p := &Poller{interval: 300 * time.Second, jitter: 0.1}
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := p.nextInterval()
		if d < 270*time.Second || d > 330*time.Second {
			t.Fatalf("interval %s out of ±10%% bounds", d)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Fatalf("jitter barely varies: %d distinct values", len(seen))
	}

	off := &Poller{interval: 300 * time.Second, jitter: 0}
	if got := off.nextInterval(); got != 300*time.Second {
		t.Fatalf("jitter disabled: got %s", got)
	}
}
