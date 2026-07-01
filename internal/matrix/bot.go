// Package matrix implements the Matrix side of the daemon: it receives !olx
// commands in the configured room and posts new-ad / price-change notifications.
package matrix

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/config"
	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"
	"github.com/ricardo-duarte-av/olx-notifier/internal/store"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Seeder lets the bot ask the poller to fetch a freshly added search right away
// (seeding it) so its first notifications don't wait two full intervals.
type Seeder interface {
	PollSearch(ctx context.Context, s store.Search)
}

// Bot is the Matrix client wrapper.
type Bot struct {
	client  *mautrix.Client
	store   *store.Store
	roomID  id.RoomID
	startTS int64 // ms; ignore messages older than this
	seeder  Seeder
	http    *http.Client
}

// New builds a Bot from config, using token authentication.
func New(cfg *config.Config, st *store.Store) (*Bot, error) {
	client, err := mautrix.NewClient(cfg.Matrix.Homeserver, id.UserID(cfg.Matrix.UserID), cfg.Matrix.AccessToken)
	if err != nil {
		return nil, err
	}
	return &Bot{
		client:  client,
		store:   st,
		roomID:  id.RoomID(cfg.Matrix.RoomID),
		startTS: time.Now().UnixMilli(),
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// SetSeeder wires the poller in (broken out to avoid a construction cycle).
func (b *Bot) SetSeeder(s Seeder) { b.seeder = s }

// Run ensures the bot is in the configured room, registers the command handler
// and blocks on sync until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	if err := b.ensureJoined(ctx); err != nil {
		return err
	}

	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		b.onMessage(ctx, evt)
	})

	return b.client.SyncWithContext(ctx)
}

// ensureJoined joins the configured room if the bot isn't already a member.
// The bot cannot function without the room, so a join failure is fatal.
func (b *Bot) ensureJoined(ctx context.Context) error {
	joined, err := b.client.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("list joined rooms: %w", err)
	}
	for _, r := range joined.JoinedRooms {
		if r == b.roomID {
			log.Printf("matrix: already in room %s", b.roomID)
			return nil
		}
	}

	log.Printf("matrix: joining room %s", b.roomID)
	if _, err := b.client.JoinRoom(ctx, b.roomID.String(), nil); err != nil {
		return fmt.Errorf("join room %s: %w", b.roomID, err)
	}
	return nil
}

func (b *Bot) onMessage(ctx context.Context, evt *event.Event) {
	// Only our room, not our own messages, and nothing from before startup.
	if evt.RoomID != b.roomID || evt.Sender == b.client.UserID || evt.Timestamp < b.startTS {
		return
	}
	body := strings.TrimSpace(evt.Content.AsMessage().Body)
	if !strings.HasPrefix(body, "!olx") {
		return
	}
	b.handleCommand(ctx, body)
}

func (b *Bot) handleCommand(ctx context.Context, body string) {
	args, err := tokenize(body)
	if err != nil {
		b.reply(ctx, "❌ "+err.Error())
		return
	}
	// args[0] == "!olx"
	if len(args) < 2 {
		b.reply(ctx, helpText())
		return
	}

	switch strings.ToLower(args[1]) {
	case "add":
		b.cmdAdd(ctx, args[2:])
	case "list", "ls":
		b.cmdList(ctx)
	case "delete", "remove", "rm", "del":
		b.cmdDelete(ctx, args[2:])
	case "disable":
		b.cmdSetEnabled(ctx, args[2:], false)
	case "enable":
		b.cmdSetEnabled(ctx, args[2:], true)
	case "help":
		b.reply(ctx, helpText())
	default:
		b.reply(ctx, helpText())
	}
}

func (b *Bot) cmdAdd(ctx context.Context, args []string) {
	// add "<query>" <min> <max> <category_id>  (min/max/category optional: - or omitted)
	if len(args) < 1 {
		b.reply(ctx, "Usage: !olx add \"<query>\" <min> <max> <category_id>")
		return
	}
	sp := olx.SearchParams{Query: args[0]}

	var perr error
	if len(args) >= 2 {
		sp.MinPrice, perr = optInt(args[1])
	}
	if perr == nil && len(args) >= 3 {
		sp.MaxPrice, perr = optInt(args[2])
	}
	if perr == nil && len(args) >= 4 {
		sp.CategoryID, perr = optInt(args[3])
	}
	if perr != nil {
		b.reply(ctx, "❌ invalid number: "+perr.Error())
		return
	}

	id, err := b.store.AddSearch(sp)
	if err != nil {
		b.reply(ctx, "❌ could not add search: "+err.Error())
		return
	}
	b.reply(ctx, fmt.Sprintf("✅ Added search #%d: %s", id, describeParams(sp)))

	// Seed it now so we have a baseline; seeding emits no notifications.
	if b.seeder != nil {
		go b.seeder.PollSearch(context.Background(), store.Search{
			ID: id, Query: sp.Query, MinPrice: sp.MinPrice, MaxPrice: sp.MaxPrice, CategoryID: sp.CategoryID,
		})
	}
}

func (b *Bot) cmdList(ctx context.Context) {
	searches, err := b.store.ListSearches()
	if err != nil {
		b.reply(ctx, "❌ "+err.Error())
		return
	}
	if len(searches) == 0 {
		b.reply(ctx, "No searches yet. Add one with !olx add \"<query>\" <min> <max> <category_id>")
		return
	}
	var sb strings.Builder
	sb.WriteString("Searches:\n")
	for _, s := range searches {
		n, _ := b.store.AdCount(s.ID)
		state := "🟢"
		if !s.Enabled {
			state = "⏸️ disabled"
		} else if !s.Seeded {
			state = "🟢 seeding…"
		}
		fmt.Fprintf(&sb, "#%d [%s] — %s — %d ads\n", s.ID, state, describeParams(s.Params()), n)
	}
	sb.WriteString("\nUse the #index with delete/disable/enable.")
	b.reply(ctx, strings.TrimRight(sb.String(), "\n"))
}

func (b *Bot) cmdDelete(ctx context.Context, args []string) {
	id, ok := b.parseIndex(ctx, args, "delete")
	if !ok {
		return
	}
	removed, err := b.store.RemoveSearch(id)
	if err != nil {
		b.reply(ctx, "❌ "+err.Error())
		return
	}
	if !removed {
		b.reply(ctx, fmt.Sprintf("No search #%d", id))
		return
	}
	b.reply(ctx, fmt.Sprintf("🗑️ Deleted search #%d and its stored results", id))
}

func (b *Bot) cmdSetEnabled(ctx context.Context, args []string, enabled bool) {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	id, ok := b.parseIndex(ctx, args, verb)
	if !ok {
		return
	}
	changed, err := b.store.SetEnabled(id, enabled)
	if err != nil {
		b.reply(ctx, "❌ "+err.Error())
		return
	}
	if !changed {
		b.reply(ctx, fmt.Sprintf("No search #%d", id))
		return
	}
	if enabled {
		b.reply(ctx, fmt.Sprintf("▶️ Enabled search #%d (re-baselining silently on next poll)", id))
	} else {
		b.reply(ctx, fmt.Sprintf("⏸️ Disabled search #%d (kept, but not searched until enabled)", id))
	}
}

// parseIndex reads a single numeric search index from args, replying with a
// usage hint on error.
func (b *Bot) parseIndex(ctx context.Context, args []string, verb string) (int64, bool) {
	if len(args) < 1 {
		b.reply(ctx, "Usage: !olx "+verb+" <index>")
		return 0, false
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(ctx, "❌ invalid index: "+args[0])
		return 0, false
	}
	return id, true
}

// photoMaxSide caps the longer edge of downloaded images.
const photoMaxSide = 1200

// Notify implements poller.Notifier: for each event it posts the ad's main photo
// with a caption (title, price, location, description), then threads any extra
// photos as replies. Ads without photos fall back to a text message.
func (b *Bot) Notify(ctx context.Context, s store.Search, events []store.Event) {
	for _, e := range events {
		b.notifyOne(ctx, s, e)
	}
}

func (b *Bot) notifyOne(ctx context.Context, s store.Search, e store.Event) {
	plain, htmlBody := formatEvent(s, e)
	photos := e.Offer.Photos

	// No photos: plain text message, nothing to thread.
	if len(photos) == 0 {
		b.replyHTML(ctx, plain, htmlBody)
		return
	}

	// Main photo carries the caption and becomes the thread root.
	rootID, err := b.sendImage(ctx, photos[0], plain, htmlBody, nil)
	if err != nil {
		log.Printf("matrix: main photo for ad %d: %v", e.Offer.ID, err)
		// Fall back to text so the notification isn't lost.
		b.replyHTML(ctx, plain, htmlBody)
		return
	}

	// Remaining photos go into a thread under the main message.
	for i, p := range photos[1:] {
		rel := (&event.RelatesTo{}).SetThread(rootID, rootID)
		caption := fmt.Sprintf("Photo %d/%d", i+2, len(photos))
		if _, err := b.sendImage(ctx, p, caption, caption, rel); err != nil {
			log.Printf("matrix: thread photo %d for ad %d: %v", i+2, e.Offer.ID, err)
		}
	}
}

// sendImage downloads a photo, uploads it to the homeserver and sends an m.image
// event with the given caption, optionally related (e.g. threaded). It returns
// the sent event's ID.
func (b *Bot) sendImage(ctx context.Context, photo olx.Photo, caption, captionHTML string, rel *event.RelatesTo) (id.EventID, error) {
	url, w, h := photo.Sized(photoMaxSide)
	data, mime, err := b.download(ctx, url)
	if err != nil {
		return "", err
	}
	up, err := b.client.UploadBytesWithName(ctx, data, mime, "olx.jpg")
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	content := event.MessageEventContent{
		MsgType:  event.MsgImage,
		Body:     caption,   // caption text (differs from FileName → treated as caption)
		FileName: "olx.jpg", // file name
		URL:      up.ContentURI.CUString(),
		Info: &event.FileInfo{
			MimeType: mime,
			Width:    w,
			Height:   h,
			Size:     len(data),
		},
		RelatesTo: rel,
	}
	if captionHTML != "" {
		content.Format = event.FormatHTML
		content.FormattedBody = captionHTML
	}

	resp, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content)
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

// download fetches an image, returning its bytes and detected content type.
func (b *Bot) download(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0")
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20 MiB cap
	if err != nil {
		return nil, "", err
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(data)
	}
	return data, mime, nil
}

func (b *Bot) reply(ctx context.Context, text string) {
	if _, err := b.client.SendText(ctx, b.roomID, text); err != nil {
		log.Printf("matrix: send: %v", err)
	}
}

func (b *Bot) replyHTML(ctx context.Context, plain, htmlBody string) {
	content := event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          plain,
		Format:        event.FormatHTML,
		FormattedBody: htmlBody,
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		log.Printf("matrix: send html: %v", err)
	}
}

// descriptionLimit caps how much of the ad description goes in the caption.
const descriptionLimit = 300

// formatEvent renders an event as a (plain, html) caption containing the header
// line, the ad link, and a truncated description.
func formatEvent(s store.Search, e store.Event) (string, string) {
	o := e.Offer
	locSuffix := ""
	if loc := o.City(); loc != "" {
		locSuffix = " · " + loc
	}

	var headPlain, headHTML string
	switch e.Type {
	case store.EventPriceChange:
		oldLabel := "?"
		if e.OldPrice != nil {
			oldLabel = strconv.Itoa(*e.OldPrice) + " €"
		}
		newLabel := o.PriceLabel()
		arrow := "💶"
		if e.OldPrice != nil {
			if p, ok := o.Price(); ok && p < *e.OldPrice {
				arrow = "📉"
			} else if ok && p > *e.OldPrice {
				arrow = "📈"
			}
		}
		headPlain = fmt.Sprintf("%s [#%d] %s — %s → %s%s",
			arrow, s.ID, o.Title, oldLabel, newLabel, locSuffix)
		headHTML = fmt.Sprintf("%s <b>[#%d]</b> <a href=%q>%s</a> — <s>%s</s> → <b>%s</b>%s",
			arrow, s.ID, o.URL, html.EscapeString(o.Title),
			html.EscapeString(oldLabel), html.EscapeString(newLabel), html.EscapeString(locSuffix))

	default: // EventNew
		headPlain = fmt.Sprintf("🆕 [#%d] %s — %s%s", s.ID, o.Title, o.PriceLabel(), locSuffix)
		headHTML = fmt.Sprintf("🆕 <b>[#%d]</b> <a href=%q>%s</a> — <b>%s</b>%s",
			s.ID, o.URL, html.EscapeString(o.Title), html.EscapeString(o.PriceLabel()),
			html.EscapeString(locSuffix))
	}

	desc := truncate(strings.TrimSpace(o.Description), descriptionLimit)

	plain := headPlain + "\n" + o.URL
	htmlBody := headHTML
	if desc != "" {
		plain += "\n\n" + desc
		htmlBody += "<br><br>" + strings.ReplaceAll(html.EscapeString(desc), "\n", "<br>")
	}
	return plain, htmlBody
}

// truncate shortens s to at most n runes, appending an ellipsis if cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

func describeParams(sp olx.SearchParams) string {
	parts := []string{fmt.Sprintf("%q", sp.Query)}
	if sp.MinPrice != nil {
		parts = append(parts, "min="+strconv.Itoa(*sp.MinPrice))
	}
	if sp.MaxPrice != nil {
		parts = append(parts, "max="+strconv.Itoa(*sp.MaxPrice))
	}
	if sp.CategoryID != nil {
		parts = append(parts, "cat="+strconv.Itoa(*sp.CategoryID))
	}
	return strings.Join(parts, " ")
}

// optInt parses an optional integer argument. Empty or "-" means "no value".
func optInt(s string) (*int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, fmt.Errorf("%q", s)
	}
	return &n, nil
}

// tokenize splits a command line honoring double-quoted segments.
func tokenize(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	hadToken := false

	flush := func() {
		if hadToken || cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
			hadToken = false
		}
	}

	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			hadToken = true
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return tokens, nil
}

func helpText() string {
	return strings.Join([]string{
		"OLX notifier commands:",
		`  !olx add "<query>" <min> <max> <category_id>  — add a search (use - to skip a filter)`,
		"  !olx list                                     — list searches with their #index and state",
		"  !olx disable <index>                          — stop searching an entry (kept in the DB)",
		"  !olx enable <index>                           — resume a disabled entry",
		"  !olx delete <index>                           — permanently delete an entry and its results",
		"  !olx help                                     — show this help",
	}, "\n")
}
