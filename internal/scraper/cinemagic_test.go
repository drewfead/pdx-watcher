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

func TestUnit_Cinemagic_ScrapeShowtimes(t *testing.T) {
	server := MountGoldenTestServer(t, "cinemagic")
	s := Cinemagic(CinemagicWithBaseURL(server.URL), CinemagicWithClient(server.Client()))
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
		assert.Equal(t, cinemagicLocation, item.Showtime.Location, "%s: Location", prefix)
		assert.False(t, item.Showtime.StartTime.IsZero(), "%s: StartTime", prefix)
		assert.NotEmpty(t, item.Showtime.Screening.Title, "%s: Screening.Title", prefix)
	}
}

func TestIntegration_Cinemagic_Showtimes(t *testing.T) {
	scrp := Cinemagic()
	showtimes, err := scrp.ScrapeShowtimes(t.Context(), internal.ListShowtimesRequest{
		After:  time.Now(),
		Before: time.Now().Add(time.Hour * 24),
	})
	require.NoError(t, err, "ScrapeShowtimes")

	for showtime := range showtimes {
		t.Logf("showtime: %+v", showtime)
	}
}
