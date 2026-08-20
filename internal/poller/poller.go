// Package poller periodically queries OLX for each stored search, reconciles
// the results against the store and forwards resulting events to a Notifier.
package poller

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
	"github.com/ricardo-duarte-av/olx-notifier/internal/store"
)

// Notifier receives events worth telling the user about.
type Notifier interface {
	Notify(ctx context.Context, s store.Search, events []store.Event)
}

// Poller ties the OLX client, the store and a Notifier together on a timer.
type Poller struct {
	store    *store.Store
	client   *olx.Client
	notifier Notifier
	interval time.Duration
	maxPages int
}

// New builds a Poller.
func New(st *store.Store, client *olx.Client, n Notifier, interval time.Duration, maxPages int) *Poller {
	return &Poller{store: st, client: client, notifier: n, interval: interval, maxPages: maxPages}
}

// Run polls all searches immediately, then on every interval tick until ctx is
// cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.pollAll(ctx)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollAll(ctx)
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
	var lastBlock *olx.StatusError

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
			var se *olx.StatusError
			if errors.As(err, &se) && se.EdgeBlocked() {
				edgeBlocked++
				lastBlock = se
				// Held back until the cycle ends: one summary beats one
				// identical block message per search.
				continue
			}
			log.Printf("poller: search %d (%q): %v", s.ID, s.Query, err)
		}
	}

	p.reportBlocks(attempted, failed, edgeBlocked, lastBlock)
}

// reportBlocks summarises CloudFront blocks for one cycle. Whether every search
// was blocked or only some is the clearest available signal for whether the
// cause is this host (IP / TLS fingerprint) or something search-specific.
func (p *Poller) reportBlocks(attempted, failed, edgeBlocked int, last *olx.StatusError) {
	if edgeBlocked == 0 {
		return
	}
	if edgeBlocked == attempted {
		log.Printf("poller: ALL %d search(es) blocked by CloudFront before reaching OLX — this is host-wide, not search-specific", attempted)
	} else {
		log.Printf("poller: %d of %d search(es) blocked by CloudFront (%d other failure(s))", edgeBlocked, attempted, failed-edgeBlocked)
	}
	log.Printf("poller: block detail: %v", last)
	if hint := last.Hint(); hint != "" {
		log.Printf("poller: %s", hint)
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
