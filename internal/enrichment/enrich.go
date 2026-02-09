package enrichment

import (
	"context"
	"log/slog"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
)

func Enrich(ctx context.Context, showtime internal.SourceShowtime, providers ...internal.EnrichmentProvider) internal.EnrichedShowtime {
	enriched := internal.EnrichedShowtime{
		Source: showtime,
		Audits: make([]internal.EnrichmentAudit, 0, len(providers)),
	}
	for _, provider := range providers {
		var err error
		enriched, err = provider.Enrich(ctx, enriched)
		if err != nil {
			enriched.Audits = append(enriched.Audits, internal.EnrichmentAudit{
				Result:      internal.EnrichmentResultFailure,
				Details:     err.Error(),
				At:          time.Now(),
				Annotations: nil,
			})
		}
	}
	for i, audit := range enriched.Audits {
		slog.Debug("enrichment audit",
			"showtime_id", showtime.ID,
			"provider_index", i,
			"result", audit.Result,
			"details", audit.Details,
			"annotations", audit.Annotations,
		)
	}
	return enriched
}
