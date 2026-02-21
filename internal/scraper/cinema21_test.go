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

func TestUnit_Cinema21_ScrapeShowtimes(t *testing.T) {
	server := MountGoldenTestServer(t, "cinema21")
	s := Cinema21(Cinema21WithBaseURL(server.URL), Cinema21WithClient(server.Client()))
	after := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

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
		assert.Equal(t, cinema21Location, item.Showtime.Location, "%s: Location", prefix)
		assert.False(t, item.Showtime.StartTime.IsZero(), "%s: StartTime", prefix)
		assert.NotEmpty(t, item.Showtime.Screening.Title, "%s: Screening.Title", prefix)
	}

	// Verify sorted by StartTime
	for i := 1; i < len(items); i++ {
		assert.False(t, items[i].Showtime.StartTime.Before(items[i-1].Showtime.StartTime),
			"items[%d] should not be before items[%d]", i, i-1)
	}
}
