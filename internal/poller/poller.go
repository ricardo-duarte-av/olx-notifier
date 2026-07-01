// Package poller periodically queries OLX for each stored search, reconciles
// the results against the store and forwards resulting events to a Notifier.
package poller

import (
	"context"
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
	for _, s := range searches {
		if ctx.Err() != nil {
			return
		}
		if !s.Enabled {
			continue
		}
		p.pollOne(ctx, s)
	}
}

// PollSearch runs a single poll for one search now. It is used to seed a search
// immediately after it is added, so the first real events arrive one interval
// later rather than after two.
func (p *Poller) PollSearch(ctx context.Context, s store.Search) {
	p.pollOne(ctx, s)
}

func (p *Poller) pollOne(ctx context.Context, s store.Search) {
	offers, err := p.client.Search(ctx, s.Params(), p.maxPages)
	if err != nil {
		log.Printf("poller: search %d (%q): %v", s.ID, s.Query, err)
		return
	}

	events, err := p.store.Reconcile(s, offers)
	if err != nil {
		log.Printf("poller: reconcile search %d: %v", s.ID, err)
		return
	}
	if len(events) == 0 {
		return
	}
	log.Printf("poller: search %d (%q): %d event(s)", s.ID, s.Query, len(events))
	p.notifier.Notify(ctx, s, events)
}
