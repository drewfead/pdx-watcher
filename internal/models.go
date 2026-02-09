package internal

import "time"

type SourceShowtime struct {
	ID           string        `json:"id"`
	Summary      string        `json:"summary"`
	Description  string        `json:"description"`
	StartTime    time.Time     `json:"start_time"`
	EndTime      time.Time     `json:"end_time"`
	Location     string        `json:"location"`
	Screening    ScreeningInfo `json:"screening"`
	TitleHint    string        `json:"title_hint"`
	DirectorHint string        `json:"director_hint,omitempty"` // from calendar-events for TMDB matching
	RuntimeHint  time.Duration `json:"runtime_hint,omitempty"`  // from calendar-events for TMDB matching (0 = unknown)
}

type EnrichedShowtime struct {
	Source SourceShowtime    `json:"showtime"`
	Movie  MovieInfo         `json:"movie"`
	Audits []EnrichmentAudit `json:"audits"`
}

type EnrichmentResult uint8

const (
	EnrichmentResultSuccess EnrichmentResult = iota
	EnrichmentResultFailure
	EnrichmentResultPartialSuccess
)

type EnrichmentAudit struct {
	Result      EnrichmentResult `json:"result"`
	Details     string           `json:"details"`
	At          time.Time        `json:"at"`
	Annotations map[string]any   `json:"annotations"`
}

type ScreeningInfo struct {
	Title       string `json:"title"`
	Subhed string `json:"subhed,omitempty"` // e.g. "in 35mm" from Hollywood listing, appended after TMDB title in summary
	Series      string `json:"series"`
	Host        string `json:"host"`
	Links       []Link `json:"links"`
}

type MovieInfo struct {
	Title   string `json:"title"`
	Tagline string `json:"tagline"`
	Overview string `json:"overview"`
	Links   []Link `json:"links"`
}

type Link struct {
	Href    string `json:"href"`
	Display string `json:"display"`
}

type ListShowtimesRequest struct {
	After  time.Time `json:"after"`
	Before time.Time `json:"before"`
	Limit  int       `json:"limit"`
	Anchor string    `json:"anchor"`
}

type ShowtimeListItem struct {
	Showtime   SourceShowtime `json:"showtime"`
	NextAnchor string         `json:"next_anchor"`
}
