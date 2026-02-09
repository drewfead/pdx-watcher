package enrichment

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	tmdb "github.com/cyruzin/golang-tmdb"
	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/httputil"
)

const maxCandidatesForDetails = 5

// httpRequestRecord is appended by auditTransport for each outgoing request.
type httpRequestRecord struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Status int    `json:"status"`
}

type auditTransport struct {
	base http.RoundTripper
	e    *tmdbEnrichment
}

func (t *auditTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.e.auditRequests != nil {
		*t.e.auditRequests = append(*t.e.auditRequests, httpRequestRecord{
			Method: req.Method,
			URL:    req.URL.String(),
			Status: resp.StatusCode,
		})
	}
	return resp, nil
}

type tmdbEnrichment struct {
	apiKey string
	client *tmdb.Client

	// Set per Enrich call for audit; cleared when done.
	auditRequests     *[]httpRequestRecord
	detailsCacheAudit *[]struct {
		MovieID  int
		CacheHit bool
	}
	cacheEvents *[]struct {
		Key string
		Hit bool
	} // every cache key + hit for this Enrich
}

// tmdbDetailsURLPat matches TMDB movie details URLs to extract movie ID for cache audit.
var tmdbDetailsURLPat = regexp.MustCompile(`/movie/(\d+)(?:\?|$)`)

func TMDB(apiKey string) (internal.EnrichmentProvider, error) {
	tmdbClient, err := tmdb.InitV4(apiKey)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize TMDB client: %w", err)
	}
	e := &tmdbEnrichment{apiKey: apiKey, client: tmdbClient}
	cacheTransport := &httputil.CacheTransport{
		Base: http.DefaultTransport,
		OnCacheHit: func(cacheKey string, hit bool) {
			e.recordCacheHit(cacheKey, hit)
		},
	}
	tmdbClient.SetClientConfig(http.Client{
		Transport: &auditTransport{base: cacheTransport, e: e},
	})
	return e, nil
}

// recordCacheHit records cache events for audit: all keys in cacheEvents, and details URLs in detailsCacheAudit.
func (e *tmdbEnrichment) recordCacheHit(cacheKey string, hit bool) {
	if e.cacheEvents != nil {
		*e.cacheEvents = append(*e.cacheEvents, struct {
			Key string
			Hit bool
		}{cacheKey, hit})
	}
	if e.detailsCacheAudit == nil {
		return
	}
	ms := tmdbDetailsURLPat.FindStringSubmatch(cacheKey)
	if len(ms) < 2 {
		return
	}
	var id int
	if _, err := fmt.Sscanf(ms[1], "%d", &id); err != nil {
		return
	}
	*e.detailsCacheAudit = append(*e.detailsCacheAudit, struct {
		MovieID  int
		CacheHit bool
	}{id, hit})
}

// searchCacheHitFromEvents returns true if the search/movie request was a cache hit.
func searchCacheHitFromEvents(events []struct {
	Key string
	Hit bool
}) bool {
	for _, ev := range events {
		if strings.Contains(ev.Key, "search/movie") {
			return ev.Hit
		}
	}
	return false
}

// titleEqual normalizes both strings (collapse spaces, case-insensitive) for comparison.
func titleEqual(a, b string) bool {
	norm := func(s string) string {
		return strings.ToUpper(strings.Join(strings.Fields(s), " "))
	}
	return norm(a) == norm(b)
}

// directorMatch returns true if the Hollywood director name matches a TMDB crew name (e.g. "Rob Reiner").
func directorMatch(hint, tmdbName string) bool {
	if hint == "" || tmdbName == "" {
		return false
	}
	return titleEqual(hint, tmdbName)
}

// runtimeDiff returns absolute difference in minutes; if expected is 0, returns 0 (no penalty).
func runtimeDiff(expectedMins int, actualMins int) int {
	if expectedMins <= 0 {
		return 0
	}
	d := actualMins - expectedMins
	if d < 0 {
		return -d
	}
	return d
}

// pickBestResult chooses the best TMDB result: when director or runtime hints exist, fetches details
// for up to maxCandidatesForDetails and prefers director match then closest runtime; otherwise
// prefers exact title match then first result.
func (e *tmdbEnrichment) pickBestResult(results []tmdb.MovieResult, normalizedHint, director string, runtimeHint time.Duration) *tmdb.MovieResult {
	if len(results) == 0 {
		return nil
	}
	runtimeMins := int(runtimeHint.Round(time.Minute).Minutes())
	// No heuristic hints: use title match or first.
	if director == "" && runtimeMins <= 0 {
		for i := range results {
			if titleEqual(results[i].Title, normalizedHint) {
				return &results[i]
			}
		}
		return &results[0]
	}

	// Fetch details for top candidates to compare director and runtime.
	n := len(results)
	if n > maxCandidatesForDetails {
		n = maxCandidatesForDetails
	}
	type scored struct {
		r    *tmdb.MovieResult
		dir  bool
		diff int
	}
	var best *scored
	for i := 0; i < n; i++ {
		details, err := e.client.GetMovieDetails(int(results[i].ID), map[string]string{"append_to_response": "credits"})
		if err != nil {
			continue
		}
		var tmdbDirector string
		if details.MovieCreditsAppend != nil && details.Credits.MovieCredits != nil {
			for _, c := range details.Credits.MovieCredits.Crew {
				if c.Job == "Director" {
					tmdbDirector = c.Name
					break
				}
			}
		}
		dirMatch := directorMatch(director, tmdbDirector)
		diff := runtimeDiff(runtimeMins, details.Runtime) // details.Runtime is minutes
		s := &scored{r: &results[i], dir: dirMatch, diff: diff}
		if best == nil {
			best = s
			continue
		}
		// Prefer director match, then smaller runtime diff.
		if s.dir && !best.dir {
			best = s
		} else if s.dir == best.dir && s.diff < best.diff {
			best = s
		}
	}
	if best != nil {
		return best.r
	}
	return &results[0]
}

func (e *tmdbEnrichment) Enrich(ctx context.Context, showtime internal.EnrichedShowtime) (internal.EnrichedShowtime, error) {
	var httpRequests []httpRequestRecord
	var detailsAudit []struct {
		MovieID  int
		CacheHit bool
	}
	var cacheEvents []struct {
		Key string
		Hit bool
	}
	e.auditRequests = &httpRequests
	e.detailsCacheAudit = &detailsAudit
	e.cacheEvents = &cacheEvents
	defer func() {
		e.auditRequests = nil
		e.detailsCacheAudit = nil
		e.cacheEvents = nil
	}()

	annotations := make(map[string]any)

	if showtime.Source.TitleHint == "" {
		annotations["skipped"] = "no title hint"
		showtime.Audits = append(showtime.Audits, internal.EnrichmentAudit{
			Result:      internal.EnrichmentResultSuccess,
			Details:     "",
			At:          time.Now(),
			Annotations: annotations,
		})
		return showtime, nil
	}

	searchTitle := showtime.Source.TitleHint
	searchResults, err := e.client.GetSearchMovies(searchTitle, map[string]string{
		"language": "en-US",
	})
	if err != nil {
		return showtime, fmt.Errorf("failed to search for movie with title hint %s: %w", searchTitle, err)
	}
	searchCacheHit := searchCacheHitFromEvents(cacheEvents)

	best := e.pickBestResult(
		searchResults.Results,
		searchTitle,
		showtime.Source.DirectorHint,
		showtime.Source.RuntimeHint,
	)
	if best != nil {
		showtime.Movie = internal.MovieInfo{
			Title:    best.Title,
			Overview: best.Overview,
			Links: []internal.Link{
				{
					Href:    fmt.Sprintf("https://www.themoviedb.org/movie/%d", best.ID),
					Display: "TMDB",
				},
			},
		}
	}

	annotations["cache_search"] = map[string]any{"hit": searchCacheHit, "query": searchTitle}
	if len(detailsAudit) > 0 {
		detailsList := make([]map[string]any, len(detailsAudit))
		for i, d := range detailsAudit {
			detailsList[i] = map[string]any{"movie_id": d.MovieID, "cache_hit": d.CacheHit}
		}
		annotations["cache_details"] = detailsList
	}
	if len(httpRequests) > 0 {
		reqs := make([]map[string]any, len(httpRequests))
		for i, r := range httpRequests {
			reqs[i] = map[string]any{"method": r.Method, "url": r.URL, "status": r.Status}
		}
		annotations["http_requests"] = reqs
	}

	showtime.Audits = append(showtime.Audits, internal.EnrichmentAudit{
		Result:      internal.EnrichmentResultSuccess,
		Details:     "",
		At:          time.Now(),
		Annotations: annotations,
	})
	return showtime, nil
}
