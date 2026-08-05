package search

import (
	"context"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/trippier/poi-api/internal/geo"
	"github.com/trippier/poi-api/pkg/types"
)

// Frame is one snapshot of a streamed search: the whole merged result as it
// stands, never a delta. Providers revise each other — a place found by two
// of them collapses into one record whose id, description and sources all
// come from the merge — so an increment could not be applied client-side
// without replaying the merge there.
type Frame struct {
	// Frame counts snapshots from 1, so a client can tell a late duplicate
	// from a fresh revision.
	Frame int `json:"frame"`
	// Partial is true while providers are still expected.
	Partial bool `json:"partial"`
	// Pending names the providers still being awaited, so a client can say
	// which source is holding things up rather than guessing.
	Pending []types.Provider `json:"pending"`
	// Failed names the providers that gave up, making a thin result
	// explainable instead of mysterious.
	Failed  []types.Provider    `json:"failed"`
	Total   int                 `json:"total"`
	Results []types.EnrichedPoi `json:"results"`
}

// StreamOptions selects which flavour of search a stream runs.
type StreamOptions struct {
	// Kind restricts the result to places or to events.
	Kind types.PointKind
	// Custom applies the caller's provider overrides and exclusions.
	Custom bool
}

// firstFrameDelay bounds how long a stream waits before its opening snapshot
// when nothing has landed yet, so a client always hears back quickly even if
// every provider is slow.
const firstFrameDelay = 1500 * time.Millisecond

/*
StreamSearch runs a search and hands each revision of the merged result to
emit, in order, until every provider has reported or the provider budget runs
out.

This is the whole point of streaming here: the merge window exists because
some providers answer in a second and others in twenty, and a single response
has to cut somewhere — today at eight seconds, which is always before
Overpass. A stream has no such need. The fast providers paint immediately and
the slow ones revise the picture when they arrive, inside the same request.

@param ctx - Request context; cancelling it ends the stream.
@param q - The search query.
@param opts - Which flavour of search to run.
@param emit - Called for each snapshot; an error stops the stream.
@returns The error emit returned, or nil once the stream is complete.
*/
func (s *Service) StreamSearch(
	ctx context.Context,
	q types.SearchQuery,
	opts StreamOptions,
	emit func(Frame) error,
) error {
	s.prepareStreamQuery(ctx, &q, opts)

	selected := s.selectProviders(q)
	pending := make(map[types.Provider]bool, len(selected))
	for _, p := range selected {
		pending[p.Name()] = true
	}

	arrivals := s.fanOut(ctx, q, selected)

	var raw []types.RawPoi
	failed := []types.Provider{}
	frame := 0

	// An opening snapshot goes out even when nothing has arrived, so the
	// client can show it is searching rather than sit on a blank list.
	opening := time.NewTimer(firstFrameDelay)
	defer opening.Stop()

	send := func() error {
		merged := s.assemble(ctx, q, raw)
		merged = filterByKind(merged, opts.Kind)
		if opts.Custom || opts.Kind == types.KindPOI {
			merged = applyFilters(merged, q)
		}
		result := paginate(merged, q)
		frame++
		if err := emit(Frame{
			Frame:   frame,
			Partial: len(pending) > 0,
			Pending: providerList(pending),
			Failed:  failed,
			Total:   result.Total,
			Results: result.Results,
		}); err != nil {
			return err
		}
		return nil
	}

	for len(pending) > 0 {
		select {
		case a := <-arrivals:
			delete(pending, a.provider)
			if a.failed {
				failed = append(failed, a.provider)
				// Nothing new to merge, but the client still deserves to know
				// this source is out rather than keep waiting on it.
				if err := send(); err != nil {
					return err
				}
				continue
			}
			raw = append(raw, a.pois...)
			if err := send(); err != nil {
				return err
			}
		case <-opening.C:
			if frame == 0 {
				if err := send(); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return nil
		}
	}

	// The last arrival emptied `pending` before its own snapshot went out, so
	// that snapshot already carried partial=false. Only a search that never
	// sent anything — every provider failing inside the opening delay — still
	// owes the client a frame.
	if frame == 0 {
		return send()
	}
	return nil
}

// prepareStreamQuery applies the same defaults and provider selection the
// non-streaming entry points do, so a stream and a plain call answer the same
// question.
func (s *Service) prepareStreamQuery(ctx context.Context, q *types.SearchQuery, opts StreamOptions) {
	if opts.Kind == types.KindEvent {
		applyDefaults(q, defaultEventProviders())
		clampRadiusToMin(q)
	} else {
		userSpecified := len(q.Providers) > 0
		applyDefaults(q, defaultProviders())
		if !userSpecified {
			if selected := s.autoSelectProviders(ctx, *q, opts.Kind); len(selected) > 0 {
				q.Providers = selected
			}
		}
	}
	if opts.Custom {
		q.Providers = filterExcluded(q.Providers, q.ExcludeProviders)
	}
	if q.Mode == types.ModeDistrict {
		if place, err := geo.GeocodeDistrict(ctx, q.District); err == nil {
			q.Lat = place.Lat
			q.Lng = place.Lng
		} else {
			s.log.Warn("geocode district failed", zap.String("district", q.District), zap.Error(err))
		}
	}
}

// providerList renders the pending set as a stable, sorted slice so two
// frames listing the same providers look the same on the wire.
func providerList(pending map[types.Provider]bool) []types.Provider {
	out := make([]types.Provider, 0, len(pending))
	for p := range pending {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
