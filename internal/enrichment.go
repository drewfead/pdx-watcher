package internal

import "context"

type EnrichmentProvider interface {
	// Enrich makes a best-effort attempt to enrich the showtime with data from the provider
	Enrich(ctx context.Context, showtime EnrichedShowtime) (EnrichedShowtime, error)
}
