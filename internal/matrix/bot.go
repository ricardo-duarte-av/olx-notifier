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
	"sync"
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
	olx     *olx.Client

	catMu sync.Mutex
	cats  []olx.Category // cached category tree (lazy-loaded)
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
		olx:     olx.NewClient(),
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
	b.handleCommand(ctx, evt.Sender, evt.ID, body)
}

func (b *Bot) handleCommand(ctx context.Context, sender id.UserID, replyTo id.EventID, body string) {
	args, err := tokenize(body)
	if err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	// args[0] == "!olx"
	if len(args) < 2 {
		b.replyCode(ctx, replyTo, helpText())
		return
	}

	switch strings.ToLower(args[1]) {
	case "add":
		b.cmdAdd(ctx, sender, replyTo, args[2:])
	case "list", "ls":
		b.cmdList(ctx, sender, replyTo)
	case "delete", "remove", "rm", "del":
		b.cmdDelete(ctx, sender, replyTo, args[2:])
	case "disable":
		b.cmdSetEnabled(ctx, sender, replyTo, args[2:], false)
	case "enable":
		b.cmdSetEnabled(ctx, sender, replyTo, args[2:], true)
	case "categories", "cat", "cats":
		b.cmdCategories(ctx, replyTo, args[2:])
	case "help":
		b.replyCode(ctx, replyTo, helpText())
	default:
		b.replyCode(ctx, replyTo, helpText())
	}
}

func (b *Bot) cmdAdd(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string) {
	// add "<query>" <min> <max> <category_id>  (min/max/category optional: - or omitted)
	if len(args) < 1 {
		b.reply(ctx, replyTo, "Usage: !olx add \"<query>\" <min> <max> <category_id>")
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
		b.reply(ctx, replyTo, "❌ invalid number: "+perr.Error())
		return
	}

	id, err := b.store.AddSearch(sp, sender.String())
	if err != nil {
		b.reply(ctx, replyTo, "❌ could not add search: "+err.Error())
		return
	}
	b.reply(ctx, replyTo, fmt.Sprintf("✅ Added search #%d: %s", id, describeParams(sp)))

	// Seed it now so we have a baseline; seeding emits no notifications.
	if b.seeder != nil {
		go b.seeder.PollSearch(context.Background(), store.Search{
			ID: id, Query: sp.Query, MinPrice: sp.MinPrice, MaxPrice: sp.MaxPrice,
			CategoryID: sp.CategoryID, Owner: sender.String(),
		})
	}
}

func (b *Bot) cmdList(ctx context.Context, sender id.UserID, replyTo id.EventID) {
	mod := b.isModerator(ctx, sender)

	var searches []store.Search
	var err error
	if mod {
		searches, err = b.store.ListSearches() // moderators see everyone's
	} else {
		searches, err = b.store.ListSearchesByOwner(sender.String())
	}
	if err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	if len(searches) == 0 {
		b.reply(ctx, replyTo, "No searches yet. Add one with !olx add \"<query>\" <min> <max> <category_id>")
		return
	}

	var sb strings.Builder
	if mod {
		sb.WriteString("All searches:\n")
	} else {
		sb.WriteString("Your searches:\n")
	}
	for _, s := range searches {
		n, _ := b.store.AdCount(s.ID)
		state := "🟢"
		if !s.Enabled {
			state = "⏸️ disabled"
		} else if !s.Seeded {
			state = "🟢 seeding…"
		}
		fmt.Fprintf(&sb, "#%d [%s] — %s — %d ads", s.ID, state, describeParams(s.Params()), n)
		if mod {
			fmt.Fprintf(&sb, " — by %s", s.Owner)
		}
		sb.WriteByte('\n')
	}
	sb.WriteString("\nUse the #index with delete/disable/enable.")
	b.replyCode(ctx, replyTo, strings.TrimRight(sb.String(), "\n"))
}

func (b *Bot) cmdDelete(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string) {
	s, ok := b.resolveOwned(ctx, sender, replyTo, args, "delete")
	if !ok {
		return
	}
	if _, err := b.store.RemoveSearch(s.ID); err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	b.reply(ctx, replyTo, fmt.Sprintf("🗑️ Deleted search #%d and its stored results", s.ID))
}

func (b *Bot) cmdSetEnabled(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string, enabled bool) {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	s, ok := b.resolveOwned(ctx, sender, replyTo, args, verb)
	if !ok {
		return
	}
	if _, err := b.store.SetEnabled(s.ID, enabled); err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return
	}
	if enabled {
		b.reply(ctx, replyTo, fmt.Sprintf("▶️ Enabled search #%d (re-baselining silently on next poll)", s.ID))
	} else {
		b.reply(ctx, replyTo, fmt.Sprintf("⏸️ Disabled search #%d (kept, but not searched until enabled)", s.ID))
	}
}

// categoryResultLimit caps how many category matches are shown.
const categoryResultLimit = 20

func (b *Bot) cmdCategories(ctx context.Context, replyTo id.EventID, args []string) {
	cats, err := b.categories(ctx)
	if err != nil {
		b.reply(ctx, replyTo, "❌ could not load categories: "+err.Error())
		return
	}
	term := strings.TrimSpace(strings.Join(args, " "))

	switch {
	case term == "":
		// No argument: list the top-level sections to browse from.
		b.replyCategoryList(ctx, replyTo,
			"Top-level categories (drill in with !olx categories <id>):",
			olx.TopLevelCategories(cats), false)

	case isInt(term):
		// Numeric argument: drill down into that category's children.
		b.cmdCategoryChildren(ctx, replyTo, cats, mustInt(term))

	default:
		// Text: keyword search by name.
		matches := olx.SearchCategories(cats, term, categoryResultLimit)
		if len(matches) == 0 {
			b.reply(ctx, replyTo, fmt.Sprintf(
				"No category named like %q. Categories are broad sections (e.g. \"computador\", "+
					"\"telemoveis\"), not product keywords — your search query already matches keywords. "+
					"Try a broader term, browse with !olx categories, or use - for no category.", term))
			return
		}
		b.replyCategoryList(ctx, replyTo,
			fmt.Sprintf("Categories matching %q:", term), matches, len(matches) >= categoryResultLimit)
	}
}

func (b *Bot) cmdCategoryChildren(ctx context.Context, replyTo id.EventID, cats []olx.Category, parentID int) {
	parent, ok := olx.CategoryByID(cats, parentID)
	if !ok {
		b.reply(ctx, replyTo, fmt.Sprintf("No category #%d. Browse with !olx categories.", parentID))
		return
	}
	children := olx.ChildCategories(cats, parentID)
	if len(children) == 0 {
		b.reply(ctx, replyTo, fmt.Sprintf(
			"%d — %s is a leaf category (no subcategories); use it directly as <category_id>.",
			parent.ID, parent.Name))
		return
	}
	if len(children) > categoryResultLimit {
		children = children[:categoryResultLimit]
	}
	b.replyCategoryList(ctx, replyTo,
		fmt.Sprintf("Subcategories of %s (#%d):", parent.Name, parent.ID),
		children, len(olx.ChildCategories(cats, parentID)) > categoryResultLimit)
}

// replyCategoryList renders a category list as a monospace reply.
func (b *Bot) replyCategoryList(ctx context.Context, replyTo id.EventID, header string, cats []olx.Category, truncated bool) {
	var sb strings.Builder
	sb.WriteString(header + "\n")
	for _, c := range cats {
		fmt.Fprintf(&sb, "%d — %s  (%s)\n", c.ID, c.Name, c.Path)
	}
	if truncated {
		sb.WriteString("… (more results not shown — refine your term)\n")
	}
	sb.WriteString("\nUse the number as <category_id> in !olx add.")
	b.replyCode(ctx, replyTo, strings.TrimRight(sb.String(), "\n"))
}

func isInt(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func mustInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// categories returns the OLX category tree, fetching and caching it on first use.
func (b *Bot) categories(ctx context.Context) ([]olx.Category, error) {
	b.catMu.Lock()
	defer b.catMu.Unlock()
	if b.cats != nil {
		return b.cats, nil
	}
	cats, err := b.olx.FetchCategories(ctx)
	if err != nil {
		return nil, err
	}
	b.cats = cats
	return cats, nil
}

// resolveOwned parses the index argument, looks up the search, and enforces that
// the sender either owns it or is a room moderator. It replies with the relevant
// error and returns ok=false when the caller should stop.
func (b *Bot) resolveOwned(ctx context.Context, sender id.UserID, replyTo id.EventID, args []string, verb string) (store.Search, bool) {
	if len(args) < 1 {
		b.reply(ctx, replyTo, "Usage: !olx "+verb+" <index>")
		return store.Search{}, false
	}
	id64, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		b.reply(ctx, replyTo, "❌ invalid index: "+args[0])
		return store.Search{}, false
	}
	s, found, err := b.store.GetSearch(id64)
	if err != nil {
		b.reply(ctx, replyTo, "❌ "+err.Error())
		return store.Search{}, false
	}
	if !found {
		b.reply(ctx, replyTo, fmt.Sprintf("No search #%d", id64))
		return store.Search{}, false
	}
	if s.Owner != sender.String() && !b.isModerator(ctx, sender) {
		b.reply(ctx, replyTo, fmt.Sprintf("🚫 Search #%d belongs to someone else (moderators can manage any search)", id64))
		return store.Search{}, false
	}
	return s, true
}

// moderatorPowerLevel is the minimum room power level treated as a moderator.
const moderatorPowerLevel = 50

// isModerator reports whether the user has moderator power (PL >= 50) in the room.
// It fetches full room state via State so the m.room.create event is wired into
// the power levels; this makes GetUserLevel honour the room-v12 rule that the
// creator has implicit infinite power despite being absent from the users map.
// On any lookup error it fails closed (returns false).
func (b *Bot) isModerator(ctx context.Context, user id.UserID) bool {
	state, err := b.client.State(ctx, b.roomID)
	if err != nil {
		log.Printf("matrix: fetch room state: %v", err)
		return false
	}
	plEvt, ok := state[event.StatePowerLevels][""]
	if !ok || plEvt == nil {
		return false
	}
	return plEvt.Content.AsPowerLevels().GetUserLevel(user) >= moderatorPowerLevel
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
	mentions := ownerMentions(s.Owner) // only the first/main message pings the owner
	photos := e.Offer.Photos

	// No photos: plain text message, nothing to thread.
	if len(photos) == 0 {
		b.replyHTML(ctx, "", plain, htmlBody, mentions)
		return
	}

	// Main photo carries the caption, pings the owner, and becomes the thread root.
	rootID, err := b.sendImage(ctx, photos[0], plain, htmlBody, nil, mentions)
	if err != nil {
		log.Printf("matrix: main photo for ad %d: %v", e.Offer.ID, err)
		// Fall back to text so the notification isn't lost.
		b.replyHTML(ctx, "", plain, htmlBody, mentions)
		return
	}

	// Remaining photos go into a thread under the main message (no extra pings).
	// The thread root stays rootID, but the reply-fallback chains to the previous
	// message in the thread so non-threaded clients render a proper reply chain.
	prevID := rootID
	for i, p := range photos[1:] {
		rel := (&event.RelatesTo{}).SetThread(rootID, prevID)
		caption := fmt.Sprintf("Photo %d/%d", i+2, len(photos))
		sentID, err := b.sendImage(ctx, p, caption, caption, rel, nil)
		if err != nil {
			log.Printf("matrix: thread photo %d for ad %d: %v", i+2, e.Offer.ID, err)
			continue
		}
		prevID = sentID
	}
}

// sendImage downloads a photo, uploads it to the homeserver and sends an m.image
// event with the given caption, optionally related (e.g. threaded) and mentioning
// users. It returns the sent event's ID.
func (b *Bot) sendImage(ctx context.Context, photo olx.Photo, caption, captionHTML string, rel *event.RelatesTo, mentions *event.Mentions) (id.EventID, error) {
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
		Mentions:  mentions,
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

// replyRelation builds an m.in_reply_to relation, or nil for a standalone message.
func replyRelation(replyTo id.EventID) *event.RelatesTo {
	if replyTo == "" {
		return nil
	}
	return (&event.RelatesTo{}).SetReplyTo(replyTo)
}

func (b *Bot) reply(ctx context.Context, replyTo id.EventID, text string) {
	content := event.MessageEventContent{
		MsgType:   event.MsgText,
		Body:      text,
		RelatesTo: replyRelation(replyTo),
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		log.Printf("matrix: send: %v", err)
	}
}

func (b *Bot) replyHTML(ctx context.Context, replyTo id.EventID, plain, htmlBody string, mentions *event.Mentions) {
	content := event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          plain,
		Format:        event.FormatHTML,
		FormattedBody: htmlBody,
		Mentions:      mentions,
		RelatesTo:     replyRelation(replyTo),
	}
	if _, err := b.client.SendMessageEvent(ctx, b.roomID, event.EventMessage, &content); err != nil {
		log.Printf("matrix: send html: %v", err)
	}
}

// replyCode sends monospace text as a reply, wrapping it in an HTML <pre><code>
// block so column alignment survives in clients that render formatted_body. The
// plain body is kept as the fallback for text-only clients.
func (b *Bot) replyCode(ctx context.Context, replyTo id.EventID, text string) {
	b.replyHTML(ctx, replyTo, text, "<pre><code>"+html.EscapeString(text)+"</code></pre>", nil)
}

// descriptionLimit caps how much of the ad description goes in the caption.
const descriptionLimit = 300

// formatEvent renders an event as a (plain, html) caption containing the header
// line, the ad link, and a truncated description.
func formatEvent(s store.Search, e store.Event) (string, string) {
	o := e.Offer
	locSuffix := ""
	if loc := o.LocationLabel(); loc != "" {
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

	desc := truncate(o.CleanDescription(), descriptionLimit)

	plain := headPlain + "\n" + o.URL
	htmlBody := headHTML
	if desc != "" {
		plain += "\n\n" + desc
		htmlBody += "<br><br>" + strings.ReplaceAll(html.EscapeString(desc), "\n", "<br>")
	}
	// Ping the search owner at the end of the caption.
	if s.Owner != "" {
		plain += "\n\n🔔 " + s.Owner
		htmlBody += "<br><br>🔔 " + mentionHTML(s.Owner)
	}
	return plain, htmlBody
}

// mentionHTML renders a Matrix user-mention pill for the given user ID.
func mentionHTML(userID string) string {
	return fmt.Sprintf("<a href=\"https://matrix.to/#/%s\">%s</a>",
		html.EscapeString(userID), html.EscapeString(userID))
}

// ownerMentions builds the m.mentions payload that actually triggers a ping.
func ownerMentions(owner string) *event.Mentions {
	if owner == "" {
		return nil
	}
	return &event.Mentions{UserIDs: []id.UserID{id.UserID(owner)}}
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
		"  !olx categories [term|id]                     — browse top-level, drill into <id>, or search by name",
		"  !olx list                                     — list searches with their #index and state",
		"  !olx disable <index>                          — stop searching an entry (kept in the DB)",
		"  !olx enable <index>                           — resume a disabled entry",
		"  !olx delete <index>                           — permanently delete an entry and its results",
		"  !olx help                                     — show this help",
	}, "\n")
}
