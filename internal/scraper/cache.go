package scraper

import (
	"context"
	"fmt"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Cached returns middleware that wraps a Scraper with LRU+TTL caching. The cache key uses
// the wrapped scraper's Descriptor(). Apply it to any scraper:
//
//	scraper.NewRegistry(scraper.WithScraperForSite(site, scraper.HollywoodTheatre(), scraper.Cached(64, 5*time.Minute)))
//
// maxEntries is the LRU size; ttl is how long entries stay valid (zero = no expiration).
func Cached(maxEntries int, ttl time.Duration) ScraperMiddleware {
	return func(inner internal.Scraper) internal.Scraper {
		if inner == nil {
			return nil
		}
		return newCachingScraper(inner, maxEntries, ttl)
	}
}

// newCachingScraper returns a Scraper that caches inner's results. Prefer using Caching middleware.
func newCachingScraper(inner internal.Scraper, maxEntries int, ttl time.Duration) internal.Scraper {
	if inner == nil {
		return nil
	}
	if maxEntries <= 0 {
		maxEntries = 64
	}
	cache := expirable.NewLRU[string, []internal.ShowtimeListItem](maxEntries, nil, ttl)
	return &cachingScraper{
		descriptor: inner.Descriptor(),
		inner:      inner,
		cache:      cache,
	}
}

// cachingScraper wraps a Scraper and caches full scrape results by request (LRU + TTL).
// The cache key is descriptor + request (after, before, limit, anchor). Only implements Scraper.
type cachingScraper struct {
	descriptor string
	inner      internal.Scraper
	cache      *expirable.LRU[string, []internal.ShowtimeListItem]
}

func cacheKey(req internal.ListShowtimesRequest) string {
	formatTime := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	return fmt.Sprintf("%s|%s|%d|%s",
		formatTime(req.After),
		formatTime(req.Before),
		req.Limit,
		req.Anchor,
	)
}

func (c *cachingScraper) Descriptor() string {
	return c.descriptor
}

func (c *cachingScraper) ScrapeShowtimes(ctx context.Context, req internal.ListShowtimesRequest) (<-chan internal.ShowtimeListItem, error) {
	key := c.descriptor + ":" + cacheKey(req)
	if list, ok := c.cache.Get(key); ok {
		ch := make(chan internal.ShowtimeListItem, len(list))
		for _, item := range list {
			ch <- item
		}
		close(ch)
		return ch, nil
	}
	ch, err := c.inner.ScrapeShowtimes(ctx, req)
	if err != nil {
		return nil, err
	}
	// Drain and cache, then replay
	var list []internal.ShowtimeListItem
	for item := range ch {
		list = append(list, item)
	}
	c.cache.Add(key, list)
	out := make(chan internal.ShowtimeListItem, len(list))
	for _, item := range list {
		out <- item
	}
	close(out)
	return out, nil
}
