// Package olx is a small client for the public OLX.pt offers API.
package olx

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// SearchParams describes a single OLX search: a free-text query plus optional
// price bounds and a category filter. Nil pointers mean "no filter".
type SearchParams struct {
	Query      string
	MinPrice   *int
	MaxPrice   *int
	CategoryID *int
}

// Offer is the subset of an OLX listing we care about.
type Offer struct {
	ID              int64   `json:"id"`
	URL             string  `json:"url"`
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	CreatedTime     string  `json:"created_time"`
	LastRefreshTime string  `json:"last_refresh_time"`
	Params          []Param `json:"params"`
	Photos          []Photo `json:"photos"`
	Location        struct {
		City struct {
			Name string `json:"name"`
		} `json:"city"`
		Region struct {
			Name string `json:"name"`
		} `json:"region"`
	} `json:"location"`
}

// Photo is one listing image. Link is a URL template containing the literal
// placeholders "{width}" and "{height}"; Width/Height are the native maximums.
type Photo struct {
	Link   string `json:"link"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Sized returns a concrete image URL plus its dimensions, scaled down so the
// longer side is at most maxSide (0 = native size). Aspect ratio is preserved.
func (p Photo) Sized(maxSide int) (url string, w, h int) {
	w, h = p.Width, p.Height
	if maxSide > 0 && (w > maxSide || h > maxSide) && w > 0 && h > 0 {
		if w >= h {
			h = h * maxSide / w
			w = maxSide
		} else {
			w = w * maxSide / h
			h = maxSide
		}
	}
	r := strings.NewReplacer("{width}", strconv.Itoa(w), "{height}", strconv.Itoa(h))
	return r.Replace(p.Link), w, h
}

// Param is a single key/value attribute on an offer (price, condition, etc.).
type Param struct {
	Key   string     `json:"key"`
	Value ParamValue `json:"value"`
}

// ParamValue is the polymorphic value of a Param. For price params, Value holds
// the numeric amount and Label the pre-formatted string (e.g. "120 €").
type ParamValue struct {
	Value json.Number `json:"value"`
	Label string      `json:"label"`
}

// Price returns the numeric price of the offer in EUR and whether one was found.
// Offers without a price (e.g. "arranged"/free) return ok=false.
func (o Offer) Price() (int, bool) {
	for _, p := range o.Params {
		if p.Key != "price" {
			continue
		}
		if p.Value.Value == "" {
			return 0, false
		}
		f, err := p.Value.Value.Float64()
		if err != nil {
			return 0, false
		}
		return int(f), true
	}
	return 0, false
}

// PriceLabel returns the human-readable price label (e.g. "120 €") if present.
func (o Offer) PriceLabel() string {
	for _, p := range o.Params {
		if p.Key == "price" {
			return p.Value.Label
		}
	}
	return ""
}

// City returns the offer's city name, if known.
func (o Offer) City() string {
	return o.Location.City.Name
}

// Region returns the offer's region/district name, if known.
func (o Offer) Region() string {
	return o.Location.Region.Name
}

// LocationLabel returns "City, Region" (matching how OLX shows it), omitting any
// missing part.
func (o Offer) LocationLabel() string {
	city, region := o.City(), o.Region()
	switch {
	case city != "" && region != "":
		return city + ", " + region
	case city != "":
		return city
	default:
		return region
	}
}

var (
	brRe    = regexp.MustCompile(`(?i)<br\s*/?>`)
	tagRe   = regexp.MustCompile(`<[^>]+>`)
	blankRe = regexp.MustCompile(`\n{3,}`)
)

// CleanDescription returns the offer description as plain text. OLX serves the
// description as HTML (<br /> line breaks, entities), so this converts <br> to
// newlines, strips any other tags, decodes entities, trims each line and
// collapses runs of blank lines.
func (o Offer) CleanDescription() string {
	s := brRe.ReplaceAllString(o.Description, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// apiResponse mirrors the top-level shape of the offers endpoint response.
type apiResponse struct {
	Data     []Offer `json:"data"`
	Metadata struct {
		TotalElements int `json:"total_elements"`
	} `json:"metadata"`
	Links struct {
		Next struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"links"`
}
