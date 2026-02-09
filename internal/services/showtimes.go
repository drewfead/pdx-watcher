package services

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/drewfead/pdx-watcher/internal"
	"github.com/drewfead/pdx-watcher/internal/enrichment"
	"github.com/drewfead/pdx-watcher/internal/scraper"
	"github.com/drewfead/pdx-watcher/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type showtimesService struct {
	registry   scraper.Registry
	enrichment []internal.EnrichmentProvider

	proto.UnimplementedShowtimeServiceServer
}

func ShowtimesService(registry scraper.Registry, enrichmentProviders ...internal.EnrichmentProvider) proto.ShowtimeServiceServer {
	return &showtimesService{
		registry:   registry,
		enrichment: enrichmentProviders,
	}
}

const defaultLimit = 100

// defaultTimeRange returns the default after (start of yesterday) and before (one year from today)
// when --after and --before are not set.
func defaultTimeRange() (after, before time.Time) {
	now := time.Now()
	loc := now.Location()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	after = startOfToday.AddDate(0, 0, -1) // start of yesterday
	before = startOfToday.AddDate(1, 0, 0) // start of the day one year from today
	return after, before
}

// protoTime returns the time if ts is set and non-zero; otherwise returns the zero time.
// Treats both Go zero (0001-01-01) and Unix epoch (1970-01-01) as unset, since the CLI
// sends epoch when --after/--before are omitted. Callers can use .IsZero() on the result.
func protoTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	t := ts.AsTime()
	if t.IsZero() || t.Equal(time.Unix(0, 0)) {
		return time.Time{}
	}
	return t
}

func (s *showtimesService) ListShowtimes(req *proto.ListShowtimesRequest, stream proto.ShowtimeService_ListShowtimesServer) error {
	scraper, err := s.registry.GetScraper(req.From.Descriptor().Syntax().GoString())
	if err != nil {
		return fmt.Errorf("unsupported site: %w", err)
	}

	limit := defaultLimit
	anchor := ""
	if req.Limit != nil {
		limit = int(*req.Limit)
	}
	if req.Anchor != nil {
		anchor = *req.Anchor
	}

	after, before := defaultTimeRange()
	if t := protoTime(req.After); !t.IsZero() {
		after = t
	}
	if t := protoTime(req.Before); !t.IsZero() {
		before = t
	}
	showtimes, err := scraper.ScrapeShowtimes(stream.Context(), internal.ListShowtimesRequest{
		After:  after,
		Before: before,
		Limit:  limit,
		Anchor: anchor,
	})
	if err != nil {
		return fmt.Errorf("failed to scrape showtimes: %w", err)
	}

	var sent int
	for showtime := range showtimes {
		enriched := enrichment.Enrich(stream.Context(), showtime.Showtime, s.enrichment...)
		resp := &proto.ListShowtimesResponse{
			Showtime: toProtoShowtime(enriched),
		}
		if showtime.NextAnchor != "" {
			resp.NextAnchor = &showtime.NextAnchor
		}
		if err := stream.Send(resp); err != nil {
			slog.Error("list-showtimes: stream.Send failed", "error", err, "sent_so_far", sent)
			return err
		}
		sent++
	}
	slog.Debug("list-showtimes", "from", req.From.String(), "sent", sent)
	return nil
}

func toProtoLinks(links []internal.Link) []*proto.Link {
	out := make([]*proto.Link, len(links))
	for i, link := range links {
		var display *string
		if link.Display != "" {
			display = &link.Display
		}
		out[i] = &proto.Link{
			Href:    link.Href,
			Display: display,
		}
	}
	return out
}

func toProtoScreeningInfo(screening internal.ScreeningInfo) *proto.ScreeningInfo {
	var title *string
	if screening.Title != "" {
		title = &screening.Title
	}
	var subhed *string
	if screening.Subhed != "" {
		subhed = &screening.Subhed
	}
	var series *string
	if screening.Series != "" {
		series = &screening.Series
	}
	var host *string
	if screening.Host != "" {
		host = &screening.Host
	}
	return &proto.ScreeningInfo{
		Title:  title,
		Subhed: subhed,
		Series: series,
		Host:   host,
		Links:  toProtoLinks(screening.Links),
	}
}

func toProtoMovieInfo(movie internal.MovieInfo) *proto.MovieInfo {
	var title *string
	if movie.Title != "" {
		title = &movie.Title
	}
	var tagline *string
	if movie.Tagline != "" {
		tagline = &movie.Tagline
	}
	var overview *string
	if movie.Overview != "" {
		overview = &movie.Overview
	}
	return &proto.MovieInfo{
		Title:    title,
		Tagline:  tagline,
		Overview: overview,
		Links:    toProtoLinks(movie.Links),
	}
}

func toProtoShowtime(showtime internal.EnrichedShowtime) *proto.Showtime {
	var description *string
	if showtime.Source.Description != "" {
		description = &showtime.Source.Description
	}
	startTime := timestamppb.New(showtime.Source.StartTime)
	endTime := timestamppb.New(showtime.Source.EndTime)
	var location *string
	if showtime.Source.Location != "" {
		location = &showtime.Source.Location
	}
	summary := showtime.Source.Summary
	if showtime.Movie.Title != "" {
		summary = showtime.Movie.Title
		if showtime.Source.Screening.Subhed != "" {
			summary += " - " + showtime.Source.Screening.Subhed
		}
	}

	return &proto.Showtime{
		Id:          showtime.Source.ID,
		Summary:     summary,
		Description: description,
		StartTime:   startTime,
		EndTime:     endTime,
		Location:    location,
		Screening:   toProtoScreeningInfo(showtime.Source.Screening),
		Movie:       toProtoMovieInfo(showtime.Movie),
	}
}
