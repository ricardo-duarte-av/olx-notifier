// This test binary makes live OLX calls, so it needs the same TLS-fingerprint
// workaround as the daemon; //go:debug applies only to the binary declaring it.
// See the note in main.go.
//
//go:debug tlssha1=1
package poller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
	"github.com/ricardo-duarte-av/olx-notifier/internal/store"
)

// recorder captures what the poller would post to the room.
type recorder struct {
	alerts []string
}

func (*recorder) Notify(context.Context, store.Search, []store.Event) {}

func (r *recorder) Alert(_ context.Context, text string) { r.alerts = append(r.alerts, text) }

// TestPollAllBlockReporting exercises the real logging path against live OLX.
// Run it with GODEBUG=tlssha1=0 to force CloudFront blocks and see the
// host-wide summary; with the default (tlssha1=1) it should poll cleanly.
//
//	go test ./internal/poller -run TestPollAllBlockReporting -v
//	GODEBUG=tlssha1=0 go test ./internal/poller -run TestPollAllBlockReporting -v
func TestPollAllBlockReporting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live network test in -short mode")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, q := range []string{"iphone", "ryzen"} {
		if _, err := st.AddSearch(olx.SearchParams{Query: q}, "@test:example.org"); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Logging and alerting are the subject under test; inspect -v output.
	rec := &recorder{}
	New(st, olx.NewClient(), rec, time.Minute, 0.1, 1).pollAll(ctx)
	for _, a := range rec.alerts {
		t.Logf("ROOM ALERT:\n%s", a)
	}
}
