package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/browser"
	"github.com/drewfead/pdx-watcher/proto"
	"github.com/go-rod/rod"
	"github.com/google/uuid"
)

type hollywoodTheatreScraper struct {
	baseURL         string
	descriptor      string // site descriptor for registry/cache key
	uuidNamespace   uuid.UUID
	httpClient      *http.Client      // non-nil = test mode (skip rod)
	headlessBrowser browser.Interface // nil = use browser.Headless()
}

// HollywoodTheatreOption applies configuration to a Hollywood Theatre scraper.
type HollywoodTheatreOption func(*hollywoodTheatreScraper)

// WithBaseURL sets the base URL for the scraper (e.g. httptest.Server.URL in tests).
func WithBaseURL(baseURL string) HollywoodTheatreOption {
	return func(s *hollywoodTheatreScraper) {
		s.baseURL = baseURL
	}
}

// WithClient sets the HTTP client for the scraper (e.g. httptest.Server.Client() in tests).
// When set, the scraper uses direct HTTP instead of headless browser.
func WithClient(client *http.Client) HollywoodTheatreOption {
	return func(s *hollywoodTheatreScraper) {
		if client != nil {
			s.httpClient = client
			s.headlessBrowser = nil
		}
	}
}

// WithBrowser injects the Browser used when scraping without an HTTP client.
func WithBrowser(b browser.Interface) HollywoodTheatreOption {
	return func(s *hollywoodTheatreScraper) {
		if b != nil {
			s.headlessBrowser = b
			s.httpClient = nil
		}
	}
}

func HollywoodTheatre(opts ...HollywoodTheatreOption) internal.Scraper {
	s := &hollywoodTheatreScraper{
		baseURL:    defaultBaseURL,
		descriptor: defaultDescriptor,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.uuidNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte(s.baseURL))
	if s.headlessBrowser == nil && s.httpClient == nil {
		s.headlessBrowser = browser.Headless()
	}
	return s
}

const (
	defaultBaseURL = "https://www.hollywoodtheatre.org"
)

var defaultDescriptor = proto.PdxSite_HollywoodTheatre.Descriptor().Syntax().GoString()

const portlandLocale = "en_US"
const portlandTimezoneCode = "America/Los_Angeles"
const hollywoodTheatreLocation = "Hollywood Theatre, 4122 NE Sandy Blvd, Portland, Oregon, 97212"

var portlandTZ *time.Location

func init() {
	var err error
	portlandTZ, err = time.LoadLocation(portlandTimezoneCode)
	if err != nil {
		portlandTZ = time.UTC
	}
}

func (s *hollywoodTheatreScraper) Descriptor() string {
	return s.descriptor
}

func (s *hollywoodTheatreScraper) ScrapeShowtimes(
	ctx context.Context,
	listReq internal.ListShowtimesRequest,
) (<-chan internal.ShowtimeListItem, error) {
	allJSON, err := s.fetchAllData(ctx, listReq)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data: %w", err)
	}

	var allShows []showEntry
	for _, view := range showListViews {
		body, ok := allJSON[view]
		if !ok {
			continue
		}
		var payload showListResponse
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("failed to unmarshal %s: %w", view, err)
		}
		allShows = append(allShows, payload.Shows...)
	}

	calendarByID := make(map[int]calendarEventDetails)
	if body, ok := allJSON["calendar-events"]; ok && len(body) > 0 {
		var err error
		calendarByID, err = parseCalendarEventsByID(body)
		if err != nil {
			// continue without calendar data
		}
	}

	slog.Debug("hollywoodtheatre: API response", "shows", len(allShows), "calendar_events", len(calendarByID))

	hits := make(chan internal.ShowtimeListItem)
	go func() {
		defer close(hits)
		s.sendShowtimes(hits, allShows, listReq, calendarByID)
	}()

	return hits, nil
}

// PullGolden fetches show-list (today, coming-soon) and calendar-events and writes golden files.
func (s *hollywoodTheatreScraper) PullGolden(ctx context.Context, goldenDir string) error {
	timeRangeStart, timeRangeEnd := goldenCalendarRange()
	allJSON, err := s.fetchAllData(ctx, internal.ListShowtimesRequest{
		After:  timeRangeStart,
		Before: timeRangeEnd,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch golden data: %w", err)
	}
	for key, body := range allJSON {
		if err := os.WriteFile(filepath.Join(goldenDir, key+".json"), body, 0o644); err != nil {
			return fmt.Errorf("failed to write %s golden file: %w", key, err)
		}
	}
	return nil
}

func (s *hollywoodTheatreScraper) MountGolden(ctx context.Context, goldenDir string) (http.Handler, error) {
	today, _ := os.ReadFile(filepath.Join(goldenDir, "today.json"))
	comingSoon, _ := os.ReadFile(filepath.Join(goldenDir, "coming-soon.json"))
	calendarEvents, _ := os.ReadFile(filepath.Join(goldenDir, "calendar-events.json"))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/wp-json/gecko-theme/v1/show-list":
			view := r.URL.Query().Get("view")
			switch view {
			case "today":
				if len(today) == 0 {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte("not found (golden file not found: today.json)"))
					return
				}
				_, _ = w.Write(today)
			case "coming-soon":
				if len(comingSoon) == 0 {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte("not found (golden file not found: coming-soon.json)"))
					return
				}
				_, _ = w.Write(comingSoon)
			default:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("invalid view"))
			}
		case "/wp-json/gecko-theme/v1/calendar-events":
			if len(calendarEvents) == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("not found (golden file not found: calendar-events.json)"))
				return
			}
			_, _ = w.Write(calendarEvents)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		}
	}), nil
}

// fetchAllData returns show-list (today, coming-soon) and calendar-events JSON for the listReq date range.
func (s *hollywoodTheatreScraper) fetchAllData(ctx context.Context, listReq internal.ListShowtimesRequest) (map[string][]byte, error) {
	if s.httpClient != nil {
		return s.fetchAllViaHTTP(ctx, listReq)
	}
	return s.fetchAllViaHeadlessBrowser(ctx, listReq)
}

// fetchAllViaHTTP fetches show-list and calendar-events (for the given listReq range).
func (s *hollywoodTheatreScraper) fetchAllViaHTTP(ctx context.Context, listReq internal.ListShowtimesRequest) (map[string][]byte, error) {
	results := make(map[string][]byte, 3)
	for _, view := range showListViews {
		req, err := http.NewRequestWithContext(ctx, "GET", s.showListURL(view, portlandLocale), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request for %s: %w", view, err)
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to get %s: %w", view, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read %s response: %w", view, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to get %s: %s", view, resp.Status)
		}
		results[view] = body
	}
	calStart, calEnd := calendarRangeFromListReq(listReq)
	calReq, err := http.NewRequestWithContext(ctx, "GET", s.calendarEventsURL(calStart.Format(time.DateOnly), calEnd.Format(time.DateOnly), portlandLocale), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create calendar-events request: %w", err)
	}
	calResp, err := s.httpClient.Do(calReq)
	if err != nil {
		return nil, fmt.Errorf("failed to get calendar-events: %w", err)
	}
	calBody, _ := io.ReadAll(calResp.Body)
	calResp.Body.Close()
	if calResp.StatusCode == http.StatusOK {
		results["calendar-events"] = calBody
	}
	return results, nil
}

// fetchAllViaHeadlessBrowser launches a headless browser and fetches show-list + calendar-events.
func (s *hollywoodTheatreScraper) fetchAllViaHeadlessBrowser(ctx context.Context, listReq internal.ListShowtimesRequest) (map[string][]byte, error) {
	var results map[string][]byte
	homeURL := s.baseURL + "/"
	err := s.headlessBrowser.WithPage(ctx, homeURL, func(page *rod.Page) error {
		calStart, calEnd := calendarRangeFromListReq(listReq)
		urls := []struct {
			key string
			url string
		}{
			{"today", s.showListURL("today", portlandLocale)},
			{"coming-soon", s.showListURL("coming-soon", portlandLocale)},
			{"calendar-events", s.calendarEventsURL(calStart.Format(time.DateOnly), calEnd.Format(time.DateOnly), portlandLocale)},
		}
		results = make(map[string][]byte, len(urls))
		for _, item := range urls {
			var raw json.RawMessage
			if err := s.headlessBrowser.FetchJSON(ctx, item.url, &raw)(page); err != nil {
				return fmt.Errorf("fetch %s: %w", item.key, err)
			}
			results[item.key] = raw
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (s *hollywoodTheatreScraper) sendShowtimes(
	hits chan<- internal.ShowtimeListItem,
	shows []showEntry,
	listReq internal.ListShowtimesRequest,
	calendarByID map[int]calendarEventDetails,
) {
	dateLayout := time.DateOnly
	timeLayout := "3:04pm" // almost time.Kitchen, but with lowercase "am/pm"

	var sent, skippedAfter, skippedBefore, skippedParse int
	for _, show := range shows {
		if show.HideEvents {
			continue
		}
		showDate, err := time.ParseInLocation(dateLayout, show.QueryDate, portlandTZ)
		if err != nil {
			skippedParse++
			continue
		}
		screeningLinks := []internal.Link{}
		if show.Permalink != "" {
			screeningLinks = append(screeningLinks, internal.Link{Href: show.Permalink, Display: "Event"})
		}
		if show.SeriesURL != "" && show.Series != "" {
			screeningLinks = append(screeningLinks, internal.Link{Href: show.SeriesURL, Display: show.Series})
		}
		normalized, subhed := s.extractTitleHintWithSubhed(show.Title)
		if normalized == "" {
			normalized = show.Title
		}
		screening := internal.ScreeningInfo{
			Title:  show.Title,
			Subhed: subhed,
			Series: show.Series,
			Links:  screeningLinks,
		}

		for _, ev := range show.Events {
			startTime, err := time.ParseInLocation(timeLayout, ev.StartTime, portlandTZ)
			if err != nil {
				skippedParse++
				continue
			}
			start := time.Date(
				showDate.Year(), showDate.Month(), showDate.Day(),
				startTime.Hour(), startTime.Minute(), 0, 0, portlandTZ,
			)
			if !listReq.After.IsZero() && !start.After(listReq.After) {
				skippedAfter++
				continue
			}
			if !listReq.Before.IsZero() && !start.Before(listReq.Before) {
				skippedBefore++
				continue
			}

			var directorHint string
			var runtimeHint time.Duration
			if cal, ok := calendarByID[ev.ID]; ok {
				directorHint = cal.DirectorHint
				runtimeHint = cal.RuntimeHint
			}

			item := internal.ShowtimeListItem{
				Showtime: internal.SourceShowtime{
					ID:           uuid.NewSHA1(s.uuidNamespace, []byte(strconv.Itoa(ev.ID))).String(),
					Summary:      show.Title,
					Description:  show.Title,
					StartTime:    start,
					Location:     hollywoodTheatreLocation,
					Screening:    screening,
					TitleHint:    normalized,
					DirectorHint: directorHint,
					RuntimeHint:  runtimeHint,
				},
			}
			hits <- item
			sent++
		}
	}
}

var showListViews = []string{"today", "coming-soon"}

// goldenCalendarRange returns start and end dates (YYYY-MM-DD) for calendar-events when pulling golden data.
// Uses Portland timezone; range is today through one year from today.
func goldenCalendarRange() (start, end time.Time) {
	now := time.Now().In(portlandTZ)
	startDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, portlandTZ)
	endDate := startDate.AddDate(1, 0, 0)
	return startDate, endDate
}

// calendarRangeFromListReq returns start_date and end_date (YYYY-MM-DD) for calendar-events from listReq.
// If listReq has zero times, uses default: today through one year from today in Portland TZ.
func calendarRangeFromListReq(listReq internal.ListShowtimesRequest) (start, end time.Time) {
	loc := portlandTZ
	now := time.Now().In(loc)
	if !listReq.After.IsZero() && !listReq.Before.IsZero() {
		return listReq.After.In(loc), listReq.Before.In(loc)
	}
	startDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	endDate := startDate.AddDate(1, 0, 0)
	return startDate, endDate
}

func (s *hollywoodTheatreScraper) showListURL(view string, locale string) string {
	u, _ := url.Parse(s.baseURL)
	u.Path = "/wp-json/gecko-theme/v1/show-list"
	q := u.Query()
	q.Set("view", view)
	q.Set("locale", locale)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *hollywoodTheatreScraper) calendarEventsURL(startDate, endDate string, locale string) string {
	u, _ := url.Parse(s.baseURL)
	u.Path = "/wp-json/gecko-theme/v1/calendar-events"
	q := u.Query()
	q.Set("start_date", startDate)
	q.Set("end_date", endDate)
	q.Set("_locale", locale)
	u.RawQuery = q.Encode()
	return u.String()
}

// parseCalendarEventsByID builds event_id -> calendarEventDetails from calendar-events JSON.
// RuntimeHint is inferred from start/end times when both are present (most reliable);
// otherwise it is parsed from the "runtime" string (e.g. "81 mins") when present.
func parseCalendarEventsByID(body []byte) (map[int]calendarEventDetails, error) {
	var resp calendarEventsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make(map[int]calendarEventDetails)
	for _, e := range resp.Events {
		for _, ev := range e.Events {
			var d time.Duration
			if ev.Start != "" && ev.End != "" {
				start, err1 := time.Parse(time.RFC3339, ev.Start)
				end, err2 := time.Parse(time.RFC3339, ev.End)
				if err1 == nil && err2 == nil && end.After(start) {
					d = end.Sub(start)
				}
			}
			if d == 0 && ev.Runtime != "" {
				var mins int
				if _, err := fmt.Sscanf(ev.Runtime, "%d mins", &mins); err == nil {
					d = time.Duration(mins) * time.Minute
				}
			}
			out[ev.EventID] = calendarEventDetails{
				DirectorHint: strings.TrimSpace(ev.Director),
				RuntimeHint:  d,
			}
		}
	}
	return out, nil
}

// showListResponse matches golden/hollywoodtheatre/coming-soon.json from
// /wp-json/gecko-theme/v1/show-list?view=coming-soon&locale=en_US
type showListResponse struct {
	Shows []showEntry `json:"shows"`
}

type showEntry struct {
	ShowPostID       string       `json:"show_post_id"`
	Title            string       `json:"title"`
	Series           string       `json:"series"`
	SeriesURL        string       `json:"series_url"`
	Permalink        string       `json:"permalink"`
	Image            string       `json:"image"`
	DisplayDate      string       `json:"display_date"`
	QueryDate        string       `json:"query_date"`
	Format           string       `json:"format"`
	DescriptiveAudio bool         `json:"descriptive_audio"`
	HideEvents       bool         `json:"hide_events"`
	Events           []eventEntry `json:"events"`
}

type eventEntry struct {
	ID        int    `json:"id"`
	StartTime string `json:"start_time"`
}

// calendarEventsResponse matches /wp-json/gecko-theme/v1/calendar-events response.
// We decode events[].events[].event_id, director, runtime, start, end for join/matching.
type calendarEventsResponse struct {
	Events []struct {
		Events []struct {
			EventID  int    `json:"event_id"`
			Director string `json:"director"`
			Runtime  string `json:"runtime"` // e.g. "81 mins", "106 mins", or ""
			Start    string `json:"start"`   // ISO8601 e.g. "2026-02-08T14:30:00+00:00"
			End      string `json:"end"`     // ISO8601
		} `json:"events"`
	} `json:"events"`
}

// calendarEventDetails is attached to each showtime when we have calendar-events data.
type calendarEventDetails struct {
	DirectorHint string
	RuntimeHint  time.Duration
}

// extractTitleHint returns a search-friendly title by stripping format and
// event suffixes that TMDB won't match (e.g. "in 70mm", "(Digital)", "with Open Captions").
func (h *hollywoodTheatreScraper) extractTitleHint(raw string) string {
	normalized, _ := h.extractTitleHintWithSubhed(raw)
	return normalized
}

// formatTerms are keywords we strip from titles and collect as subhed (display-only).
var formatTerms = []string{"70mm", "35mm", "16mm", "8mm", "Digital"}

// titleSuffixes are " in X" and " (X)" for each format term, built at init.
var titleSuffixes []string

func init() {
	for _, t := range formatTerms {
		titleSuffixes = append(titleSuffixes, " in "+t, " ("+t+")")
	}
}

var trailingParenRE = regexp.MustCompile(`\s*\(([^)]+)\)\s*$`)

// stripTrailingParen removes a single trailing "(...)" from s if the content is a format term or 4-digit year.
// Returns (trimmed s, content to add to subhed, true) or (s, "", false).
func stripTrailingParen(s string) (string, string, bool) {
	loc := trailingParenRE.FindStringSubmatchIndex(s)
	if loc == nil {
		return s, "", false
	}
	inner := strings.TrimSpace(s[loc[2]:loc[3]])
	innerUpper := strings.ToUpper(inner)
	for _, t := range formatTerms {
		if innerUpper == strings.ToUpper(t) {
			return strings.TrimSpace(s[:loc[0]]), inner, true
		}
	}
	if len(inner) == 4 && isDigits(inner) {
		return strings.TrimSpace(s[:loc[0]]), inner, true
	}
	return s, "", false
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// extractTitleHintWithSubhed returns a TMDB-search-friendly title and a subhed of stripped parts
// (e.g. "in 35mm", "with Open Captions") joined by " - ", for display as "Title - in 35mm".
func (h *hollywoodTheatreScraper) extractTitleHintWithSubhed(raw string) (titleHint, subhed string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	var parts []string

	// Strip " with ..." so we can then strip format suffixes from the end.
	if i := strings.Index(strings.ToUpper(s), " WITH "); i > 0 {
		parts = append(parts, strings.TrimSpace(s[i:]))
		s = strings.TrimSpace(s[:i])
	}

	for {
		unchanged := true

		// Strip one format suffix (" in 35mm", " (Digital)", etc.)
		upper := strings.ToUpper(s)
		for _, suf := range titleSuffixes {
			if strings.HasSuffix(upper, strings.ToUpper(suf)) {
				stripped := strings.TrimSpace(s[len(s)-len(suf):])
				if len(stripped) >= 2 && stripped[0] == '(' && stripped[len(stripped)-1] == ')' {
					stripped = strings.TrimSpace(stripped[1 : len(stripped)-1])
				}
				parts = append(parts, stripped)
				s = strings.TrimSpace(s[:len(s)-len(suf)])
				unchanged = false
				break
			}
		}
		if unchanged {
			// Strip one trailing (...) if it's a format term or year.
			if trimmed, content, ok := stripTrailingParen(s); ok {
				parts = append(parts, content)
				s = trimmed
				unchanged = false
			}
		}
		if unchanged {
			break
		}
	}

	return strings.TrimSpace(s), strings.Join(parts, " - ")
}
