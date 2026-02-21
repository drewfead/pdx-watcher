package scraper

import (
	"context"
	"testing"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/stretchr/testify/require"
)

// mockScraper returns a fixed list of items and implements internal.Scraper.
type mockScraper struct {
	descriptor string
	items      []internal.ShowtimeListItem
}

func (m *mockScraper) Descriptor() string { return m.descriptor }
func (m *mockScraper) ScrapeShowtimes(
	ctx context.Context,
	_ internal.ListShowtimesRequest,
) (<-chan internal.ShowtimeListItem, error) {
	ch := make(chan internal.ShowtimeListItem)
	go func() {
		defer close(ch)
		for _, it := range m.items {
			select {
			case ch <- it:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func TestUnit_Interleaved_MergesByStartTime(t *testing.T) {
	t1 := time.Date(2026, 2, 20, 19, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 20, 21, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 2, 20, 20, 0, 0, 0, time.UTC)
	t4 := time.Date(2026, 2, 20, 22, 0, 0, 0, time.UTC)

	a := &mockScraper{
		descriptor: "A",
		items: []internal.ShowtimeListItem{
			{Showtime: internal.SourceShowtime{ID: "a1", Summary: "A first", StartTime: t1}},
			{Showtime: internal.SourceShowtime{ID: "a2", Summary: "A second", StartTime: t2}},
		},
	}
	b := &mockScraper{
		descriptor: "B",
		items: []internal.ShowtimeListItem{
			{Showtime: internal.SourceShowtime{ID: "b1", Summary: "B mid", StartTime: t3}},
			{Showtime: internal.SourceShowtime{ID: "b2", Summary: "B last", StartTime: t4}},
		},
	}

	merged := Interleaved(a, b)
	require.Equal(t, "interleaved:A,B", merged.Descriptor())

	ch, err := merged.ScrapeShowtimes(context.Background(), internal.ListShowtimesRequest{
		After:  t1.Add(-time.Hour),
		Before: t4.Add(time.Hour),
	})
	require.NoError(t, err)

	var got []internal.ShowtimeListItem
	for it := range ch {
		got = append(got, it)
	}
	require.Len(t, got, 4)
	require.Equal(t, "a1", got[0].Showtime.ID, "earliest is a1 at 19:00")
	require.Equal(t, "b1", got[1].Showtime.ID, "then b1 at 20:00")
	require.Equal(t, "a2", got[2].Showtime.ID, "then a2 at 21:00")
	require.Equal(t, "b2", got[3].Showtime.ID, "then b2 at 22:00")
}

func TestUnit_Interleaved_SingleScraperReturnsAsIs(t *testing.T) {
	a := &mockScraper{descriptor: "A", items: []internal.ShowtimeListItem{
		{Showtime: internal.SourceShowtime{ID: "only", Summary: "Only", StartTime: time.Now()}},
	}}
	merged := Interleaved(a)
	require.Equal(t, "A", merged.Descriptor())
	ch, err := merged.ScrapeShowtimes(context.Background(), internal.ListShowtimesRequest{})
	require.NoError(t, err)
	var got []internal.ShowtimeListItem
	for it := range ch {
		got = append(got, it)
	}
	require.Len(t, got, 1)
	require.Equal(t, "only", got[0].Showtime.ID)
}

func TestUnit_Interleaved_RespectsLimit(t *testing.T) {
	t1 := time.Date(2026, 2, 20, 19, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 20, 20, 0, 0, 0, time.UTC)
	a := &mockScraper{descriptor: "A", items: []internal.ShowtimeListItem{
		{Showtime: internal.SourceShowtime{ID: "1", StartTime: t1}},
		{Showtime: internal.SourceShowtime{ID: "2", StartTime: t2}},
	}}
	b := &mockScraper{descriptor: "B", items: []internal.ShowtimeListItem{}}

	merged := Interleaved(a, b)
	ch, err := merged.ScrapeShowtimes(context.Background(), internal.ListShowtimesRequest{Limit: 1})
	require.NoError(t, err)
	var got []internal.ShowtimeListItem
	for it := range ch {
		got = append(got, it)
	}
	require.Len(t, got, 1)
	require.Equal(t, "1", got[0].Showtime.ID)
}
