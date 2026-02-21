package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/browser"
	"github.com/drewfead/pdx-watcher/proto"
	"github.com/go-rod/rod"
	"github.com/google/uuid"
)

type cinemagicScraper struct {
	baseURL         string
	descriptor      string
	uuidNamespace   uuid.UUID
	httpClient      *http.Client
	headlessBrowser browser.Interface
}

// CinemagicOption applies configuration to a Cinemagic scraper.
type CinemagicOption func(*cinemagicScraper)

// CinemagicWithBaseURL sets the base URL for the scraper (e.g. httptest.Server.URL in tests).
func CinemagicWithBaseURL(baseURL string) CinemagicOption {
	return func(s *cinemagicScraper) {
		s.baseURL = baseURL
	}
}

// CinemagicWithClient sets the HTTP client for the scraper (e.g. httptest.Server.Client() in tests).
// When set, the scraper uses direct HTTP instead of headless browser.
func CinemagicWithClient(client *http.Client) CinemagicOption {
	return func(s *cinemagicScraper) {
		if client != nil {
			s.httpClient = client
			s.headlessBrowser = nil
		}
	}
}

// CinemagicWithBrowser injects the Browser used when scraping without an HTTP client.
func CinemagicWithBrowser(b browser.Interface) CinemagicOption {
	return func(s *cinemagicScraper) {
		if b != nil {
			s.headlessBrowser = b
			s.httpClient = nil
		}
	}
}

func Cinemagic(opts ...CinemagicOption) internal.Scraper {
	s := &cinemagicScraper{
		baseURL:    defaultCinemagicBaseURL,
		descriptor: cinemagicDescriptor,
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
	defaultCinemagicBaseURL = "https://tickets.thecinemagictheater.com"
	cinemagicLocation       = "Cinemagic, 2021 SE Hawthorne Blvd, Portland, Oregon, 97214"
	cinemagicSiteIDInt      = 40
	cinemagicCircuitID      = "39"
	cinemagicSiteID         = "40"
)

var cinemagicDescriptor = proto.PdxSite_Cinemagic.Descriptor().Syntax().GoString()

var (
	errGraphQLRequestFailed          = errors.New("graphql request failed")
	errUnexpectedDatesResponseFormat = errors.New("unmarshal datesWithShowing: unexpected format")
)

const cinemagicDatesQuery = `query ($siteIds: [ID]) {
  datesWithShowing(siteIds: $siteIds) {
    value
  }
}`

const cinemagicShowingsQuery = `query ($date: String, $siteIds: [ID]) {
  showingsForDate(date: $date, siteIds: $siteIds) {
    data {
      id
      time
      published
      past
      displayMetaData
      movie {
        id
        name
        synopsis
        starring
        directedBy
        duration
        genre
        allGenres
        rating
        tmdbId
        urlSlug
      }
    }
    count
  }
}`

// fetchPostJSONScript sends a POST GraphQL request from the page context with the required
// INDY Cinema Group headers (circuit-id, site-id, client-type). Without these the API returns 403.
const fetchPostJSONScript = `(url, body, circuitID, siteID) => {
	return fetch(url, {
		method: 'POST',
		headers: {
			'Content-Type': 'application/json',
			'circuit-id': circuitID,
			'site-id': siteID,
			'client-type': 'consumer',
			'is-electron-mode': 'false'
		},
		credentials: 'include',
		body: body
	}).then(r => {
		if (!r.ok) throw new Error('HTTP ' + r.status);
		return r.json();
	}).then(obj => JSON.stringify(obj));
}`

// waitForCookieScript polls document.cookie until the target cookie name appears (max ~10s).
// This ensures the SPA has fully initialized and server cookies are established.
const waitForCookieScript = `(cookieName) => {
	return new Promise((resolve, reject) => {
		let tries = 0;
		const check = () => {
			if (document.cookie.includes(cookieName + '=')) {
				resolve(true);
			} else if (tries++ > 100) {
				reject(new Error('cookie ' + cookieName + ' not found after 10s'));
			} else {
				setTimeout(check, 100);
			}
		};
		check();
	});
}`

func (s *cinemagicScraper) Descriptor() string {
	return s.descriptor
}

func (s *cinemagicScraper) ScrapeShowtimes(
	ctx context.Context,
	listReq internal.ListShowtimesRequest,
) (<-chan internal.ShowtimeListItem, error) {
	_, allJSON, err := s.fetchShowings(ctx, listReq)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data: %w", err)
	}

	hits := make(chan internal.ShowtimeListItem)
	go func() {
		defer close(hits)
		s.sendShowtimes(hits, allJSON, listReq)
	}()

	return hits, nil
}

// PullGolden fetches showings for 7 days starting today, saving dates.json and {date}.json per date.
func (s *cinemagicScraper) PullGolden(ctx context.Context, goldenDir string) error {
	now := time.Now().In(portlandTZ)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, portlandTZ)
	end := start.AddDate(0, 0, 7)

	datesResp, allJSON, err := s.fetchShowings(ctx, internal.ListShowtimesRequest{
		After:  start,
		Before: end,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch golden data: %w", err)
	}
	allJSON["dates"] = datesResp
	return writeGoldenFiles(goldenDir, allJSON)
}

func (s *cinemagicScraper) MountGolden(_ context.Context, goldenDir string) (http.Handler, error) {
	// Read datesWithShowing golden response.
	datesResp, err := os.ReadFile(filepath.Join(goldenDir, "dates.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to read dates golden file: %w", err)
	}

	// Read per-date golden files into memory keyed by date.
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read golden dir: %w", err)
	}
	goldenByDate := make(map[string][]byte)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "dates.json" {
			continue
		}
		dateStr := strings.TrimSuffix(e.Name(), ".json")
		data, err := os.ReadFile(filepath.Join(goldenDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to read golden file %s: %w", e.Name(), err)
		}
		goldenByDate[dateStr] = data
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad request"))
			return
		}

		// Route by query: datesWithShowing vs showingsForDate.
		if strings.Contains(string(body), "datesWithShowing") {
			_, _ = w.Write(datesResp)
			return
		}

		var req struct {
			Variables struct {
				Date string `json:"date"`
			} `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("bad request"))
			return
		}
		data, ok := goldenByDate[req.Variables.Date]
		if !ok {
			_, _ = w.Write([]byte(`{"data":{"showingsForDate":{"data":[],"count":0}}}`))
			return
		}
		_, _ = w.Write(data)
	}), nil
}

// fetchShowings queries datesWithShowing first to discover which dates have data,
// filters to the requested range (if any), then fetches showingsForDate for each.
// Returns the raw datesWithShowing response and a map of date→showings response.
func (s *cinemagicScraper) fetchShowings(ctx context.Context, listReq internal.ListShowtimesRequest) ([]byte, map[string][]byte, error) {
	if s.httpClient != nil {
		return s.fetchShowingsViaHTTP(ctx, listReq)
	}
	return s.fetchShowingsViaHeadlessBrowser(ctx, listReq)
}

func (s *cinemagicScraper) datesRequestBody() ([]byte, error) {
	return json.Marshal(map[string]any{
		"query": cinemagicDatesQuery,
		"variables": map[string]any{
			"siteIds": []int{cinemagicSiteIDInt},
		},
	})
}

func (s *cinemagicScraper) showingsRequestBody(date string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"query": cinemagicShowingsQuery,
		"variables": map[string]any{
			"date":    date,
			"siteIds": []int{cinemagicSiteIDInt},
		},
	})
}

func (s *cinemagicScraper) graphqlURL() string {
	u, _ := url.Parse(s.baseURL)
	u.Path = "/graphql"
	return u.String()
}

// filterDatesToRange keeps only dates within the After/Before window.
// Dates are YYYY-MM-DD strings compared in Portland timezone.
func filterDatesToRange(dates []string, listReq internal.ListShowtimesRequest) []string {
	if listReq.After.IsZero() && listReq.Before.IsZero() {
		return dates
	}
	// Convert boundaries to date-only in Portland TZ for inclusive comparison.
	var afterDate, beforeDate string
	if !listReq.After.IsZero() {
		afterDate = listReq.After.In(portlandTZ).Add(-24 * time.Hour).Format(time.DateOnly)
	}
	if !listReq.Before.IsZero() {
		beforeDate = listReq.Before.In(portlandTZ).Add(24 * time.Hour).Format(time.DateOnly)
	}
	var filtered []string
	for _, d := range dates {
		if afterDate != "" && d < afterDate {
			continue
		}
		if beforeDate != "" && d > beforeDate {
			continue
		}
		filtered = append(filtered, d)
	}
	return filtered
}

// postGraphQL sends a GraphQL request via httpClient and returns the response body.
func (s *cinemagicScraper) postGraphQL(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.graphqlURL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Circuit-Id", cinemagicCircuitID)
	req.Header.Set("Site-Id", cinemagicSiteID)
	req.Header.Set("Client-Type", "consumer")
	req.Header.Set("Is-Electron-Mode", "false")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", errGraphQLRequestFailed, resp.Status)
	}
	return respBody, nil
}

// evalGraphQL executes a GraphQL POST from within the page context via fetch().
func evalGraphQL(ctx context.Context, page *rod.Page, gqlURL string, body []byte) ([]byte, error) {
	result, err := page.Context(ctx).Timeout(browser.PageStableTimeout).Eval(
		fetchPostJSONScript, gqlURL, string(body), cinemagicCircuitID, cinemagicSiteID,
	)
	if err != nil {
		return nil, err
	}
	return []byte(result.Value.Str()), nil
}

// parseDatesResponse extracts date strings from a datesWithShowing GraphQL response.
// The API returns {data: {datesWithShowing: {value: "[\"2026-02-20\",...]", resultVersion: "..."}}}
// where the value field is a JSON-encoded string array.
func parseDatesResponse(body []byte) ([]string, error) {
	var resp struct {
		Data struct {
			DatesWithShowing struct {
				Value string `json:"value"`
			} `json:"datesWithShowing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal datesWithShowing envelope: %w", err)
	}
	var dates []string
	if err := json.Unmarshal([]byte(resp.Data.DatesWithShowing.Value), &dates); err != nil {
		return nil, fmt.Errorf("%w: %s", errUnexpectedDatesResponseFormat, resp.Data.DatesWithShowing.Value)
	}
	return dates, nil
}

func (s *cinemagicScraper) fetchShowingsViaHTTP(ctx context.Context, listReq internal.ListShowtimesRequest) ([]byte, map[string][]byte, error) {
	// 1. Discover available dates.
	datesBody, err := s.datesRequestBody()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal datesWithShowing: %w", err)
	}
	datesResp, err := s.postGraphQL(ctx, datesBody)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch datesWithShowing: %w", err)
	}
	allDates, err := parseDatesResponse(datesResp)
	if err != nil {
		return nil, nil, err
	}
	dates := filterDatesToRange(allDates, listReq)
	slog.Debug("cinemagic: dates to fetch", "available", len(allDates), "filtered", len(dates))

	// 2. Fetch showings for each date.
	results := make(map[string][]byte, len(dates))
	for _, date := range dates {
		body, err := s.showingsRequestBody(date)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal showingsForDate %s: %w", date, err)
		}
		resp, err := s.postGraphQL(ctx, body)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch showingsForDate %s: %w", date, err)
		}
		results[date] = resp
	}
	return datesResp, results, nil
}

func (s *cinemagicScraper) fetchShowingsViaHeadlessBrowser(ctx context.Context, listReq internal.ListShowtimesRequest) ([]byte, map[string][]byte, error) {
	var (
		rawDatesResp []byte
		results      map[string][]byte
	)
	homeURL := s.baseURL + "/"
	gqlURL := s.graphqlURL()

	err := s.headlessBrowser.WithPage(ctx, homeURL, func(page *rod.Page) error {
		// Wait for the Ahoy visit cookie — set by the SPA's JS after full initialization.
		if _, err := page.Context(ctx).Timeout(browser.PageStableTimeout).Eval(waitForCookieScript, "ahoy_visit"); err != nil {
			slog.Warn("cinemagic: cookie wait failed, proceeding anyway", "error", err)
		}

		// 1. Discover available dates.
		datesBody, err := s.datesRequestBody()
		if err != nil {
			return fmt.Errorf("marshal datesWithShowing: %w", err)
		}
		datesResp, err := evalGraphQL(ctx, page, gqlURL, datesBody)
		if err != nil {
			return fmt.Errorf("fetch datesWithShowing: %w", err)
		}
		rawDatesResp = datesResp
		allDates, err := parseDatesResponse(datesResp)
		if err != nil {
			return err
		}
		dates := filterDatesToRange(allDates, listReq)
		slog.Debug("cinemagic: dates to fetch", "available", len(allDates), "filtered", len(dates))

		// 2. Fetch showings for each date.
		results = make(map[string][]byte, len(dates))
		for _, date := range dates {
			body, err := s.showingsRequestBody(date)
			if err != nil {
				return fmt.Errorf("marshal showingsForDate %s: %w", date, err)
			}
			resp, err := evalGraphQL(ctx, page, gqlURL, body)
			if err != nil {
				return fmt.Errorf("fetch showingsForDate %s: %w", date, err)
			}
			results[date] = resp
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return rawDatesResp, results, nil
}

func (s *cinemagicScraper) sendShowtimes(
	hits chan<- internal.ShowtimeListItem,
	allJSON map[string][]byte,
	listReq internal.ListShowtimesRequest,
) {
	var items []internal.ShowtimeListItem
	var skipped int
	for dateStr, body := range allJSON {
		var resp cinemagicGraphQLResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			slog.Warn("cinemagic: failed to unmarshal response", "date", dateStr, "error", err)
			continue
		}
		for _, showing := range resp.Data.ShowingsForDate.Data {
			if !showing.Published {
				continue
			}
			startTime, err := time.Parse(time.RFC3339, showing.Time)
			if err != nil {
				skipped++
				continue
			}
			if !listReq.After.IsZero() && !startTime.After(listReq.After) {
				skipped++
				continue
			}
			if !listReq.Before.IsZero() && !startTime.Before(listReq.Before) {
				skipped++
				continue
			}

			var endTime time.Time
			if showing.Movie.Duration > 0 {
				endTime = startTime.Add(time.Duration(showing.Movie.Duration) * time.Minute)
			}

			var subhed string
			if showing.DisplayMetaData != "" {
				var meta cinemagicDisplayMeta
				if err := json.Unmarshal([]byte(showing.DisplayMetaData), &meta); err == nil && meta.Classes != "" {
					subhed = meta.Classes
				}
			}

			var links []internal.Link
			if showing.Movie.URLSlug != "" {
				links = append(links, internal.Link{
					Href:    s.baseURL + "/movie/" + showing.Movie.URLSlug,
					Display: "Tickets",
				})
			}

			items = append(items, internal.ShowtimeListItem{
				Showtime: internal.SourceShowtime{
					ID:          uuid.NewSHA1(s.uuidNamespace, []byte(showing.ID)).String(),
					Summary:     showing.Movie.Name,
					Description: showing.Movie.Synopsis,
					StartTime:   startTime,
					EndTime:     endTime,
					Location:    cinemagicLocation,
					Screening: internal.ScreeningInfo{
						Title:  showing.Movie.Name,
						Subhed: subhed,
						Links:  links,
					},
					TitleHint:    showing.Movie.Name,
					DirectorHint: showing.Movie.DirectedBy,
					RuntimeHint:  time.Duration(showing.Movie.Duration) * time.Minute,
				},
				Site: proto.PdxSite_Cinemagic,
			})
		}
	}

	slices.SortFunc(items, func(a, b internal.ShowtimeListItem) int {
		return a.Showtime.StartTime.Compare(b.Showtime.StartTime)
	})
	for _, item := range items {
		hits <- item
	}
	slog.Debug("cinemagic: emitted showtimes", "sent", len(items), "skipped", skipped)
}

// cinemagicGraphQLResponse is the top-level GraphQL response.
type cinemagicGraphQLResponse struct {
	Data struct {
		ShowingsForDate struct {
			Data  []cinemagicShowing `json:"data"`
			Count int                `json:"count"`
		} `json:"showingsForDate"`
	} `json:"data"`
}

type cinemagicShowing struct {
	ID              string         `json:"id"`
	Time            string         `json:"time"`
	Published       bool           `json:"published"`
	Past            bool           `json:"past"`
	DisplayMetaData string         `json:"displayMetaData"`
	Movie           cinemagicMovie `json:"movie"`
}

type cinemagicMovie struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Synopsis   string `json:"synopsis"`
	Starring   string `json:"starring"`
	DirectedBy string `json:"directedBy"`
	Duration   int    `json:"duration"`
	Genre      string `json:"genre"`
	AllGenres  string `json:"allGenres"`
	Rating     string `json:"rating"`
	TMDBId     string `json:"tmdbId"`
	URLSlug    string `json:"urlSlug"`
}

type cinemagicDisplayMeta struct {
	Classes string `json:"classes"`
}
