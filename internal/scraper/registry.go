package scraper

import (
	"errors"
	"fmt"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/proto"
)

type Registry interface {
	GetScraper(descriptor string) (internal.Scraper, error)
	// AllSites returns the list of PdxSite values that have a scraper registered (excluding None).
	// Used when --from is omitted to build an interleaved scraper for all theaters.
	AllSites() []proto.PdxSite
}

type ScraperMiddleware func(internal.Scraper) internal.Scraper

type RegistryOption func(r *registry)

func NewRegistry(opts ...RegistryOption) Registry {
	r := &registry{
		scrapers: make(map[string]internal.Scraper),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func WithScraper(descriptor string, scraper internal.Scraper, middleware ...ScraperMiddleware) RegistryOption {
	return func(r *registry) {
		for _, m := range middleware {
			scraper = m(scraper)
		}
		r.scrapers[descriptor] = scraper
	}
}

func WithScraperForSite(site proto.PdxSite, scraper internal.Scraper, middleware ...ScraperMiddleware) RegistryOption {
	return func(r *registry) {
		WithScraper(site.String(), scraper, middleware...)(r)
		if site != proto.PdxSite_None {
			r.allSites = append(r.allSites, site)
		}
	}
}

type registry struct {
	scrapers map[string]internal.Scraper
	allSites []proto.PdxSite
}

func (r *registry) AllSites() []proto.PdxSite {
	if len(r.allSites) == 0 {
		return nil
	}
	out := make([]proto.PdxSite, len(r.allSites))
	copy(out, r.allSites)
	return out
}

var ErrScraperNotFound = errors.New("scraper not found")

func (r *registry) GetScraper(descriptor string) (internal.Scraper, error) {
	scraper, ok := r.scrapers[descriptor]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrScraperNotFound, descriptor)
	}
	return scraper, nil
}
