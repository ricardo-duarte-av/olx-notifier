// Command olx-notifier is a long-lived daemon that watches OLX.pt searches and
// posts new listings and price changes into a Matrix room. Searches are managed
// at runtime with !olx commands in that room.
//
// Go 1.25 changed the crypto/tls default to tlssha1=0, which drops the SHA-1
// signature algorithms from the ClientHello. That shifts the TLS fingerprint
// enough that OLX's CloudFront WAF answers every API request with HTTP 403.
// Restoring the pre-1.25 default keeps the handshake acceptable to it.
//
//go:debug tlssha1=1
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/config"
	"github.com/ricardo-duarte-av/olx-notifier/internal/matrix"
	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
	"github.com/ricardo-duarte-av/olx-notifier/internal/poller"
	"github.com/ricardo-duarte-av/olx-notifier/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	bot, err := matrix.New(cfg, st)
	if err != nil {
		log.Fatalf("matrix: %v", err)
	}

	p := poller.New(st, olx.NewClient(), bot,
		time.Duration(cfg.Poll.IntervalSeconds)*time.Second, cfg.Poll.Jitter(), cfg.Poll.MaxPages)
	bot.SetSeeder(p)

	// Cancel everything on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go p.Run(ctx)

	log.Printf("olx-notifier started (interval=%ds ±%.0f%%, db=%s)",
		cfg.Poll.IntervalSeconds, cfg.Poll.Jitter()*100, cfg.DBPath)
	if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("matrix sync: %v", err)
	}
	log.Println("shutting down")
}
