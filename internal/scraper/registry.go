package scraper

import (
	"errors"
	"fmt"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/proto"
)

type Registry interface {
	GetScraper(descriptor string) (internal.Scraper, error)
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
		WithScraper(site.Descriptor().Syntax().GoString(), scraper, middleware...)(r)
	}
}

type registry struct {
	scrapers map[string]internal.Scraper
}

var ErrScraperNotFound = errors.New("scraper not found")

func (r *registry) GetScraper(descriptor string) (internal.Scraper, error) {
	scraper, ok := r.scrapers[descriptor]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrScraperNotFound, descriptor)
	}
	return scraper, nil
}
