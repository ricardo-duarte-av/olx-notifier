// Package store persists searches and the ads seen for each of them in SQLite,
// and computes new/price-change events when reconciling fresh results.
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/ricardo-duarte-av/olx-notifier/internal/olx"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registers "sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Search is a stored search definition.
type Search struct {
	ID         int64
	Query      string
	MinPrice   *int
	MaxPrice   *int
	CategoryID *int
	Seeded     bool
	Enabled    bool
}

// Params converts a stored Search into olx.SearchParams.
func (s Search) Params() olx.SearchParams {
	return olx.SearchParams{
		Query:      s.Query,
		MinPrice:   s.MinPrice,
		MaxPrice:   s.MaxPrice,
		CategoryID: s.CategoryID,
	}
}

// EventType distinguishes a brand-new ad from a price change.
type EventType int

const (
	// EventNew is a listing not previously seen for this search.
	EventNew EventType = iota
	// EventPriceChange is a previously seen listing whose price changed.
	EventPriceChange
)

// Event is something worth notifying about.
type Event struct {
	Type     EventType
	Offer    olx.Offer
	OldPrice *int // set only for EventPriceChange
}

// Open opens (and migrates) the SQLite database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection keeps this small daemon free of lock contention.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS searches (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  query       TEXT,
  min_price   INTEGER,
  max_price   INTEGER,
  category_id INTEGER,
  seeded      INTEGER NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS seen_ads (
  search_id  INTEGER NOT NULL,
  ad_id      INTEGER NOT NULL,
  price      INTEGER,
  title      TEXT,
  url        TEXT,
  first_seen TEXT NOT NULL,
  last_seen  TEXT NOT NULL,
  PRIMARY KEY (search_id, ad_id),
  FOREIGN KEY (search_id) REFERENCES searches(id) ON DELETE CASCADE
);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Add columns introduced after the initial schema for existing databases.
	return s.addColumnIfMissing("searches", "enabled", "INTEGER NOT NULL DEFAULT 1")
}

// addColumnIfMissing adds a column to a table if it does not already exist.
func (s *Store) addColumnIfMissing(table, column, def string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	return err
}

// AddSearch inserts a new search and returns its id.
func (s *Store) AddSearch(sp olx.SearchParams) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO searches (query, min_price, max_price, category_id, seeded, created_at)
		 VALUES (?, ?, ?, ?, 0, ?)`,
		sp.Query, nullInt(sp.MinPrice), nullInt(sp.MaxPrice), nullInt(sp.CategoryID),
		time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RemoveSearch deletes a search and (via cascade) its seen ads. It reports
// whether a row was actually removed.
func (s *Store) RemoveSearch(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM searches WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetEnabled enables or disables a search. Re-enabling also resets the seeded
// flag so the next poll silently re-baselines the search: ads posted while it
// was disabled are absorbed without a burst of notifications. It reports whether
// a row was affected.
func (s *Store) SetEnabled(id int64, enabled bool) (bool, error) {
	var res sql.Result
	var err error
	if enabled {
		res, err = s.db.Exec(`UPDATE searches SET enabled = 1, seeded = 0 WHERE id = ?`, id)
	} else {
		res, err = s.db.Exec(`UPDATE searches SET enabled = 0 WHERE id = ?`, id)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListSearches returns all stored searches ordered by id.
func (s *Store) ListSearches() ([]Search, error) {
	rows, err := s.db.Query(
		`SELECT id, query, min_price, max_price, category_id, seeded, enabled
		 FROM searches ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Search
	for rows.Next() {
		var (
			se              Search
			min, max, cat   sql.NullInt64
			seeded, enabled int
		)
		if err := rows.Scan(&se.ID, &se.Query, &min, &max, &cat, &seeded, &enabled); err != nil {
			return nil, err
		}
		se.MinPrice = toPtr(min)
		se.MaxPrice = toPtr(max)
		se.CategoryID = toPtr(cat)
		se.Seeded = seeded != 0
		se.Enabled = enabled != 0
		out = append(out, se)
	}
	return out, rows.Err()
}

// AdCount returns how many ads are stored for a search.
func (s *Store) AdCount(searchID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM seen_ads WHERE search_id = ?`, searchID).Scan(&n)
	return n, err
}

// Reconcile diffs the freshly fetched offers against what's stored for the
// search, updating the store and returning events to notify about. On the very
// first reconcile of a search (seeded=0) it stores everything silently and
// returns no events, so adding a search never floods the room.
func (s *Store) Reconcile(search Search, offers []olx.Offer) ([]Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Load current prices for this search.
	existing := map[int64]*int{}
	rows, err := tx.Query(`SELECT ad_id, price FROM seen_ads WHERE search_id = ?`, search.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var price sql.NullInt64
		if err := rows.Scan(&id, &price); err != nil {
			rows.Close()
			return nil, err
		}
		existing[id] = toPtr(price)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seeding := !search.Seeded
	now := time.Now().UTC().Format(time.RFC3339)
	var events []Event

	for _, o := range offers {
		price, hasPrice := o.Price()
		var newPrice *int
		if hasPrice {
			p := price
			newPrice = &p
		}

		old, seen := existing[o.ID]
		switch {
		case !seen:
			if err := upsertAd(tx, search.ID, o, newPrice, now, true); err != nil {
				return nil, err
			}
			if !seeding {
				events = append(events, Event{Type: EventNew, Offer: o})
			}
		case priceChanged(old, newPrice):
			if err := upsertAd(tx, search.ID, o, newPrice, now, false); err != nil {
				return nil, err
			}
			if !seeding {
				events = append(events, Event{Type: EventPriceChange, Offer: o, OldPrice: old})
			}
		default:
			// Unchanged: just bump last_seen.
			if _, err := tx.Exec(
				`UPDATE seen_ads SET last_seen = ? WHERE search_id = ? AND ad_id = ?`,
				now, search.ID, o.ID); err != nil {
				return nil, err
			}
		}
	}

	if seeding {
		if _, err := tx.Exec(`UPDATE searches SET seeded = 1 WHERE id = ?`, search.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func upsertAd(tx *sql.Tx, searchID int64, o olx.Offer, price *int, now string, insert bool) error {
	if insert {
		_, err := tx.Exec(
			`INSERT INTO seen_ads (search_id, ad_id, price, title, url, first_seen, last_seen)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			searchID, o.ID, nullInt(price), o.Title, o.URL, now, now)
		return err
	}
	_, err := tx.Exec(
		`UPDATE seen_ads SET price = ?, title = ?, url = ?, last_seen = ?
		 WHERE search_id = ? AND ad_id = ?`,
		nullInt(price), o.Title, o.URL, now, searchID, o.ID)
	return err
}

// priceChanged reports whether the price differs, treating "no price" as a
// distinct state from any numeric price.
func priceChanged(old, new *int) bool {
	if (old == nil) != (new == nil) {
		return true
	}
	if old == nil {
		return false
	}
	return *old != *new
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}

func toPtr(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
