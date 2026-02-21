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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/browser"
	"github.com/drewfead/pdx-watcher/proto"
	"github.com/go-rod/rod"
	"github.com/google/uuid"
)

type cinema21Scraper struct {
	baseURL         string
	descriptor      string
	uuidNamespace   uuid.UUID
	httpClient      *http.Client
	headlessBrowser browser.Interface
}

// Cinema21Option applies configuration to a Cinema 21 scraper.
type Cinema21Option func(*cinema21Scraper)

// Cinema21WithBaseURL sets the base URL for the scraper (e.g. httptest.Server.URL in tests).
func Cinema21WithBaseURL(baseURL string) Cinema21Option {
	return func(s *cinema21Scraper) {
		s.baseURL = baseURL
	}
}

// Cinema21WithClient sets the HTTP client for the scraper (e.g. httptest.Server.Client() in tests).
// When set, the scraper uses direct HTTP instead of headless browser.
func Cinema21WithClient(client *http.Client) Cinema21Option {
	return func(s *cinema21Scraper) {
		if client != nil {
			s.httpClient = client
			s.headlessBrowser = nil
		}
	}
}

// Cinema21WithBrowser injects the Browser used when scraping without an HTTP client.
func Cinema21WithBrowser(b browser.Interface) Cinema21Option {
	return func(s *cinema21Scraper) {
		if b != nil {
			s.headlessBrowser = b
			s.httpClient = nil
		}
	}
}

func Cinema21(opts ...Cinema21Option) internal.Scraper {
	s := &cinema21Scraper{
		baseURL:    defaultCinema21BaseURL,
		descriptor: cinema21Descriptor,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.uuidNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte(s.baseURL))
	// Cinema 21's API is accessible via plain HTTP — no browser needed by default.
	if s.headlessBrowser == nil && s.httpClient == nil {
		s.httpClient = http.DefaultClient
	}
	return s
}

const (
	defaultCinema21BaseURL = "https://www.cinema21.com"
	cinema21Location       = "Cinema 21, 616 NW 21st Ave, Portland, Oregon, 97209"
)

var cinema21Descriptor = proto.PdxSite_Cinema21.Descriptor().Syntax().GoString()

func (s *cinema21Scraper) Descriptor() string {
	return s.descriptor
}

func (s *cinema21Scraper) ScrapeShowtimes(
	ctx context.Context,
	listReq internal.ListShowtimesRequest,
) (<-chan internal.ShowtimeListItem, error) {
	data, err := s.fetchPlayingNow(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data: %w", err)
	}

	hits := make(chan internal.ShowtimeListItem)
	go func() {
		defer close(hits)
		s.sendShowtimes(hits, data, listReq)
	}()

	return hits, nil
}

// PullGolden fetches playing-now and saves it as golden data.
func (s *cinema21Scraper) PullGolden(ctx context.Context, goldenDir string) error {
	data, err := s.fetchPlayingNow(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch golden data: %w", err)
	}
	return writeGoldenFiles(goldenDir, map[string][]byte{
		"playing-now": data,
	})
}

func (s *cinema21Scraper) MountGolden(_ context.Context, goldenDir string) (http.Handler, error) {
	playingNow, err := os.ReadFile(filepath.Join(goldenDir, "playing-now.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to read playing-now golden file: %w", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/movie/playing-now" && r.Method == http.MethodGet {
			_, _ = w.Write(playingNow)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}), nil
}

func (s *cinema21Scraper) playingNowURL() string {
	u, _ := url.Parse(s.baseURL)
	u.Path = "/api/movie/playing-now"
	return u.String()
}

func (s *cinema21Scraper) fetchPlayingNow(ctx context.Context) ([]byte, error) {
	if s.httpClient != nil {
		return s.fetchPlayingNowViaHTTP(ctx)
	}
	return s.fetchPlayingNowViaHeadlessBrowser(ctx)
}

func (s *cinema21Scraper) fetchPlayingNowViaHTTP(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.playingNowURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get playing-now: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", errHTTPRequestFailed, resp.Status)
	}
	return body, nil
}

func (s *cinema21Scraper) fetchPlayingNowViaHeadlessBrowser(ctx context.Context) ([]byte, error) {
	var result []byte
	homeURL := s.baseURL + "/"
	apiURL := s.playingNowURL()

	err := s.headlessBrowser.WithPage(ctx, homeURL, func(page *rod.Page) error {
		var raw json.RawMessage
		if err := s.headlessBrowser.FetchJSON(ctx, apiURL, &raw)(page); err != nil {
			return fmt.Errorf("fetch playing-now: %w", err)
		}
		result = raw
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *cinema21Scraper) sendShowtimes(
	hits chan<- internal.ShowtimeListItem,
	data []byte,
	listReq internal.ListShowtimesRequest,
) {
	var movies []cinema21Movie
	if err := json.Unmarshal(data, &movies); err != nil {
		slog.Warn("cinema21: failed to unmarshal playing-now", "error", err)
		return
	}

	timeLayout := "3:04pm"
	var items []internal.ShowtimeListItem
	var skipped int
	for _, movie := range movies {
		duration, _ := strconv.Atoi(movie.Duration)
		runtimeHint := time.Duration(duration) * time.Minute

		var directorHint string
		if movie.DirectorInfo != nil && len(movie.DirectorInfo.Director) > 0 {
			directorHint = strings.Join(movie.DirectorInfo.Director, ", ")
		}

		for _, session := range movie.SessionTimes {
			sessionDate, err := time.ParseInLocation(time.DateOnly, session.Date, portlandTZ)
			if err != nil {
				skipped++
				continue
			}
			sessionTime, err := time.ParseInLocation(timeLayout, strings.ToLower(session.Time), portlandTZ)
			if err != nil {
				skipped++
				continue
			}
			start := time.Date(
				sessionDate.Year(), sessionDate.Month(), sessionDate.Day(),
				sessionTime.Hour(), sessionTime.Minute(), 0, 0, portlandTZ,
			)

			if !listReq.After.IsZero() && !start.After(listReq.After) {
				skipped++
				continue
			}
			if !listReq.Before.IsZero() && !start.Before(listReq.Before) {
				skipped++
				continue
			}

			var endTime time.Time
			if duration > 0 {
				endTime = start.Add(runtimeHint)
			}

			var links []internal.Link
			if movie.URL != "" {
				links = append(links, internal.Link{
					Href:    s.baseURL + "/movie/" + movie.URL,
					Display: "Info",
				})
			}
			if session.BookingLink != "" {
				links = append(links, internal.Link{
					Href:    session.BookingLink,
					Display: "Tickets",
				})
			}

			items = append(items, internal.ShowtimeListItem{
				Showtime: internal.SourceShowtime{
					ID:          uuid.NewSHA1(s.uuidNamespace, []byte(session.ID)).String(),
					Summary:     movie.Title,
					Description: stripHTMLTags(movie.SynopsisShort),
					StartTime:   start,
					EndTime:     endTime,
					Location:    cinema21Location,
					Screening: internal.ScreeningInfo{
						Title: movie.Title,
						Links: links,
					},
					TitleHint:    movie.Title,
					DirectorHint: directorHint,
					RuntimeHint:  runtimeHint,
				},
				Site: proto.PdxSite_Cinema21,
			})
		}
	}

	slices.SortFunc(items, func(a, b internal.ShowtimeListItem) int {
		return a.Showtime.StartTime.Compare(b.Showtime.StartTime)
	})
	for _, item := range items {
		hits <- item
	}
	slog.Debug("cinema21: emitted showtimes", "sent", len(items), "skipped", skipped)
}

// stripHTMLTags removes HTML tags from a string for use as plain-text description.
func stripHTMLTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// cinema21Movie represents a movie from the /api/movie/playing-now response.
type cinema21Movie struct {
	URL           string              `json:"url"`
	Title         string              `json:"title"`
	Reference     string              `json:"reference"`
	Duration      string              `json:"duration"`
	Classification string             `json:"classification"`
	ReleaseDate   string              `json:"releaseDate"`
	SynopsisShort string              `json:"synopsisShort"`
	Cast          []string            `json:"cast"`
	DirectorInfo  *cinema21Director   `json:"director"`
	SessionTimes  []cinema21Session   `json:"sessionTimes"`
	Trailer       string              `json:"trailer"`
}

// cinema21Director is a nested object in the movie response that contains director names.
type cinema21Director struct {
	Director []string `json:"director"`
}

// cinema21Session represents a single showtime session.
type cinema21Session struct {
	Date        string `json:"date"`
	Time        string `json:"time"`
	BookingLink string `json:"bookingLink"`
	IsSoldOut   bool   `json:"isSoldOut"`
	ID          string `json:"_id"`
}
