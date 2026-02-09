package scraper

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_HollywoodTheatre_ScrapeShowtimes(t *testing.T) {
	server := MountGoldenTestServer(t, "hollywoodtheatre")
	s := HollywoodTheatre(WithBaseURL(server.URL), WithClient(server.Client()))
	after := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	ch, err := s.ScrapeShowtimes(context.Background(), internal.ListShowtimesRequest{
		After:  after,
		Before: before,
	})
	require.NoError(t, err, "ScrapeShowtimes")

	var items []internal.ShowtimeListItem
	for item := range ch {
		items = append(items, item)
	}
	require.NotEmpty(t, items, "expected at least one showtime from golden data")

	for i, item := range items {
		t.Logf("item: %+v", item)
		prefix := fmt.Sprintf("items[%d]", i)
		assert.NotEmpty(t, item.Showtime.ID, "%s: ID", prefix)
		assert.NotEmpty(t, item.Showtime.Summary, "%s: Summary", prefix)
		assert.Equal(t, "Hollywood Theatre, 4122 NE Sandy Blvd, Portland, Oregon, 97212", item.Showtime.Location, "%s: Location", prefix)
		assert.False(t, item.Showtime.StartTime.IsZero(), "%s: StartTime", prefix)
		assert.NotEmpty(t, item.Showtime.Screening.Title, "%s: Screening.Title", prefix)
	}
}

func TestNormalizeTitleHint(t *testing.T) {
	h := HollywoodTheatre().(*hollywoodTheatreScraper)
	tests := []struct {
		name     string
		raw      string
		expected string
	}{
		{"empty", "", ""},
		{"no change", "PARIS BLUES", "PARIS BLUES"},
		{"in 35mm", "SHOGUN ASSASSIN in 35mm", "SHOGUN ASSASSIN"},
		{"in 70mm", "MALCOLM X in 70mm", "MALCOLM X"},
		{"(Digital)", "MARTY SUPREME (Digital)", "MARTY SUPREME"},
		{"(Digital) and space", "THE TESTAMENT OF ANN LEE (Digital)", "THE TESTAMENT OF ANN LEE"},
		{"with Open Captions", "THE TESTAMENT OF ANN LEE with Open Captions", "THE TESTAMENT OF ANN LEE"},
		{"with guest", "SOUTHLAND TALES with Brett Weldele", "SOUTHLAND TALES"},
		{"with Bruce Campbell", "ERNIE AND EMMA with Bruce Campbell", "ERNIE AND EMMA"},
		{"combined format then with", "SOME MOVIE in 70mm with Open Captions", "SOME MOVIE"},
		{"year in parens", "SOME MOVIE (2024)", "SOME MOVIE"},
		{"series style parens kept", "SOME MOVIE (Part One)", "SOME MOVIE (Part One)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.extractTitleHint(tt.raw)
			if got != tt.expected {
				t.Errorf("NormalizeTitleHint(%q) = %q, want %q", tt.raw, got, tt.expected)
			}
		})
	}
}

func TestNormalizeTitleHintWithSuffix(t *testing.T) {
	h := HollywoodTheatre().(*hollywoodTheatreScraper)
	tests := []struct {
		name           string
		raw            string
		wantNormalized string
		wantSuffix     string
	}{
		{"empty", "", "", ""},
		{"no suffix", "PARIS BLUES", "PARIS BLUES", ""},
		{"in 35mm", "WOMAN IN THE DUNES in 35mm", "WOMAN IN THE DUNES", "in 35mm"},
		{"in 70mm", "MALCOLM X in 70mm", "MALCOLM X", "in 70mm"},
		{"(Digital)", "MARTY SUPREME (Digital)", "MARTY SUPREME", "Digital"},
		{"with Open Captions", "THE TESTAMENT OF ANN LEE with Open Captions", "THE TESTAMENT OF ANN LEE", "with Open Captions"},
		{"combined", "SOME MOVIE in 70mm with Open Captions", "SOME MOVIE", "with Open Captions - in 70mm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNorm, gotSuffix := h.extractTitleHintWithSubhed(tt.raw)
			if gotNorm != tt.wantNormalized || gotSuffix != tt.wantSuffix {
				t.Errorf("NormalizeTitleHintWithSuffix(%q) = %q, %q; want %q, %q",
					tt.raw, gotNorm, gotSuffix, tt.wantNormalized, tt.wantSuffix)
			}
		})
	}
}

func TestIntegration_HollywoodTheatre_Showtimes(t *testing.T) {
	scrp := HollywoodTheatre()
	showtimes, err := scrp.ScrapeShowtimes(t.Context(), internal.ListShowtimesRequest{
		After:  time.Now(),
		Before: time.Now().Add(time.Hour * 24),
	})
	require.NoError(t, err, "ScrapeShowtimes")

	for showtime := range showtimes {
		t.Logf("showtime: %+v", showtime)
	}
}
