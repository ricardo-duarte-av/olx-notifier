// Package poller periodically queries OLX for each stored search, reconciles
// the results against the store and forwards resulting events to a Notifier.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
	"github.com/ricardo-duarte-av/olx-notifier/internal/store"
)

// Notifier receives events worth telling the user about.
type Notifier interface {
	Notify(ctx context.Context, s store.Search, events []store.Event)

	// Alert posts an unprompted operational warning to the room. The poller
	// uses it when polling is broken outright, which the room would otherwise
	// only notice as silence.
	Alert(ctx context.Context, text string)
}

// alertRepeat is how long to wait before repeating an ongoing outage alert, so
// a long block does not post on every tick.
const alertRepeat = time.Hour

// Poller ties the OLX client, the store and a Notifier together on a timer.
type Poller struct {
	store    *store.Store
	client   *olx.Client
	notifier Notifier
	interval time.Duration
	jitter   float64 // fraction of interval, e.g. 0.1 for ±10%
	maxPages int

	// Outage tracking, so a total failure is announced once rather than every
	// tick, and recovery is announced too.
	outageSince time.Time
	lastAlertAt time.Time
}

// New builds a Poller. jitter spreads each tick by ±that fraction of interval.
func New(st *store.Store, client *olx.Client, n Notifier, interval time.Duration, jitter float64, maxPages int) *Poller {
	return &Poller{store: st, client: client, notifier: n, interval: interval, jitter: jitter, maxPages: maxPages}
}

// nextInterval returns the interval with jitter applied, so repeated polls do
// not land on a perfectly predictable beat.
func (p *Poller) nextInterval() time.Duration {
	if p.jitter <= 0 {
		return p.interval
	}
	spread := float64(p.interval) * p.jitter
	d := float64(p.interval) + (rand.Float64()*2-1)*spread
	if min := float64(time.Second); d < min {
		d = min
	}
	return time.Duration(d)
}

// Run polls all searches immediately, then on every interval tick until ctx is
// cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.pollAll(ctx)

	// A timer rather than a ticker: each wait is re-jittered.
	t := time.NewTimer(p.nextInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollAll(ctx)
			t.Reset(p.nextInterval())
		}
	}
}

func (p *Poller) pollAll(ctx context.Context) {
	searches, err := p.store.ListSearches()
	if err != nil {
		log.Printf("poller: list searches: %v", err)
		return
	}

	var attempted, failed, edgeBlocked int
	var lastErr error

	for _, s := range searches {
		if ctx.Err() != nil {
			return
		}
		if !s.Enabled {
			continue
		}
		attempted++

		if err := p.pollOne(ctx, s); err != nil {
			failed++
			lastErr = err

			var se *olx.StatusError
			if errors.As(err, &se) && se.EdgeBlocked() {
				edgeBlocked++
				// Held back until the cycle ends: one summary beats one
				// identical block message per search.
				continue
			}
			log.Printf("poller: search %d (%q): %v", s.ID, s.Query, err)
		}
	}

	p.reportHealth(ctx, attempted, failed, edgeBlocked, lastErr)
}

// reportHealth summarises one cycle. Whether every search failed or only some
// is the clearest available signal for whether the cause is this host (IP / TLS
// fingerprint, network) or something specific to one search.
func (p *Poller) reportHealth(ctx context.Context, attempted, failed, edgeBlocked int, lastErr error) {
	if attempted == 0 {
		// Nothing to poll is not an outage; drop any stale state so a later
		// success does not announce a recovery that never happened.
		p.outageSince = time.Time{}
		p.lastAlertAt = time.Time{}
		return
	}

	if failed < attempted {
		p.reportRecovery(ctx)
		if edgeBlocked > 0 {
			log.Printf("poller: %d of %d search(es) blocked by CloudFront (%d other failure(s))",
				edgeBlocked, attempted, failed-edgeBlocked)
			logDetail(lastErr)
		}
		return
	}

	// Nothing got through this cycle.
	first := p.outageSince.IsZero()
	if first {
		p.outageSince = time.Now()
	}
	if edgeBlocked == attempted {
		log.Printf("poller: ALL %d search(es) blocked by CloudFront before reaching OLX — this is host-wide, not search-specific", attempted)
	} else {
		log.Printf("poller: ALL %d search(es) failed (%d blocked by CloudFront)", attempted, edgeBlocked)
	}
	logDetail(lastErr)

	if first || time.Since(p.lastAlertAt) >= alertRepeat {
		p.lastAlertAt = time.Now()
		p.alert(ctx, outageText(attempted, edgeBlocked, time.Since(p.outageSince), lastErr))
	}
}

// reportRecovery announces the end of a total outage, if one was in progress.
func (p *Poller) reportRecovery(ctx context.Context) {
	if p.outageSince.IsZero() {
		return
	}
	down := time.Since(p.outageSince).Round(time.Second)
	p.outageSince = time.Time{}
	p.lastAlertAt = time.Time{}
	log.Printf("poller: recovered; polling was failing for %s", down)
	p.alert(ctx, fmt.Sprintf("✅ OLX polling recovered (was failing for %s).", down))
}

// outageText renders the room-facing warning for a total outage.
func outageText(attempted, edgeBlocked int, down time.Duration, lastErr error) string {
	var b strings.Builder
	if down < time.Minute {
		fmt.Fprintf(&b, "⚠️ OLX polling is failing: all %d search(es) could not be checked.", attempted)
	} else {
		fmt.Fprintf(&b, "⚠️ OLX polling is still failing: all %d search(es) could not be checked for %s.",
			attempted, down.Round(time.Minute))
	}
	if edgeBlocked == attempted {
		b.WriteString(" Every request was blocked by CloudFront before reaching OLX, so this is host-wide rather than a problem with any one search.")
	}
	if lastErr != nil {
		fmt.Fprintf(&b, "\n\nDetail: %v", lastErr)
	}
	var se *olx.StatusError
	if errors.As(lastErr, &se) {
		if hint := se.Hint(); hint != "" {
			fmt.Fprintf(&b, "\n\n%s", hint)
		}
	}
	b.WriteString("\n\nNew listings will be missed until this clears; I will post again when it recovers.")
	return b.String()
}

// alert posts to the room, tolerating a nil notifier (tests).
func (p *Poller) alert(ctx context.Context, text string) {
	if p.notifier == nil {
		return
	}
	p.notifier.Alert(ctx, text)
}

// logDetail prints the representative error plus any operator hint.
func logDetail(err error) {
	if err == nil {
		return
	}
	log.Printf("poller: failure detail: %v", err)
	var se *olx.StatusError
	if errors.As(err, &se) {
		if hint := se.Hint(); hint != "" {
			log.Printf("poller: %s", hint)
		}
	}
}

// PollSearch runs a single poll for one search now. It is used to seed a search
// immediately after it is added, so the first real events arrive one interval
// later rather than after two.
func (p *Poller) PollSearch(ctx context.Context, s store.Search) {
	if err := p.pollOne(ctx, s); err != nil {
		log.Printf("poller: search %d (%q): %v", s.ID, s.Query, err)
		var se *olx.StatusError
		if errors.As(err, &se) {
			if hint := se.Hint(); hint != "" {
				log.Printf("poller: %s", hint)
			}
		}
	}
}

// pollOne polls a single search. It returns the error rather than logging it so
// the caller can tell a host-wide block apart from a one-off failure.
func (p *Poller) pollOne(ctx context.Context, s store.Search) error {
	offers, err := p.client.Search(ctx, s.Params(), p.maxPages)
	if err != nil {
		return err
	}

	events, err := p.store.Reconcile(s, offers)
	if err != nil {
		log.Printf("poller: reconcile search %d: %v", s.ID, err)
		return nil
	}
	if len(events) == 0 {
		return nil
	}
	log.Printf("poller: search %d (%q): %d event(s)", s.ID, s.Query, len(events))
	p.notifier.Notify(ctx, s, events)
	return nil
}
