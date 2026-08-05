package search

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/trippier/poi-api/pkg/types"
)

// ndjsonContentType is one JSON document per line, which suits a response
// whose length is unknown when the first byte goes out.
const ndjsonContentType = "application/x-ndjson"

// wantsStream reports whether the caller asked for a streamed answer, either
// with ?stream=true or by negotiating the NDJSON media type.
//
// Streaming is a delivery choice, not a different resource: the content is
// the same search. That is why it rides the request rather than the path,
// where it would have doubled the route table and doubled it again for the
// next transport-level option.
//
// @param c - The incoming request.
// @returns Whether to answer with a stream.
func wantsStream(c *gin.Context) bool {
	if v := c.Query("stream"); v != "" {
		on, err := strconv.ParseBool(v)
		return err == nil && on
	}
	return strings.Contains(c.GetHeader("Accept"), ndjsonContentType)
}

// streamSearch answers a search as a sequence of NDJSON snapshots.
//
// Each line is the complete merged result as it stood when a provider
// reported, so the client replaces its list rather than reconciling records —
// merging stays server-side, where the provider priorities and the dedup
// rules live.
//
// @param c - The request being answered.
// @param q - The parsed search query.
// @param opts - Which flavour of search to run.
func (h *Handler) streamSearch(c *gin.Context, q types.SearchQuery, opts StreamOptions) {
	c.Header("Content-Type", ndjsonContentType)
	c.Header("Cache-Control", "no-store")
	// Proxies that buffer would defeat the point by holding every frame back
	// until the stream closed.
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	encoder := json.NewEncoder(c.Writer)
	err := h.service.StreamSearch(allByokContext(c), q, opts, func(frame Frame) error {
		if err := encoder.Encode(frame); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	})
	if err != nil {
		// The status line is long gone, so a failure mid-stream can only be
		// reported in-band; the client sees a frame that never turns final.
		_ = encoder.Encode(map[string]string{"error": "stream interrupted"})
		c.Writer.Flush()
	}
}

// streamSlim answers a search as NDJSON carrying the lightweight projection.
//
// @param c - The request being answered.
// @param q - The parsed search query.
// @param opts - Which flavour of search to run.
func (h *Handler) streamSlim(c *gin.Context, q types.SearchQuery, opts StreamOptions) {
	c.Header("Content-Type", ndjsonContentType)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	encoder := json.NewEncoder(c.Writer)
	err := h.service.StreamSearch(allByokContext(c), q, opts, func(frame Frame) error {
		slim := make([]types.SlimPoi, len(frame.Results))
		for i, p := range frame.Results {
			slim[i] = types.SlimPoi{Name: p.Name, Type: p.Type, Coords: p.Coords}
		}
		if err := encoder.Encode(slimFrame{
			Frame:   frame.Frame,
			Partial: frame.Partial,
			Pending: frame.Pending,
			Failed:  frame.Failed,
			Total:   frame.Total,
			Results: slim,
		}); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	})
	if err != nil {
		_ = encoder.Encode(map[string]string{"error": "stream interrupted"})
		c.Writer.Flush()
	}
}

// streamEventsSlim answers an event search as NDJSON carrying the event
// projection, which unlike the POI one keeps the dates.
//
// @param c - The request being answered.
// @param q - The parsed search query.
// @param opts - Which flavour of search to run.
func (h *Handler) streamEventsSlim(c *gin.Context, q types.SearchQuery, opts StreamOptions) {
	c.Header("Content-Type", ndjsonContentType)
	c.Header("Cache-Control", "no-store")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	encoder := json.NewEncoder(c.Writer)
	err := h.service.StreamSearch(allByokContext(c), q, opts, func(frame Frame) error {
		slim := make([]types.SlimEvent, len(frame.Results))
		for i, e := range frame.Results {
			slim[i] = types.SlimEvent{
				Name:      e.Name,
				Coords:    e.Coords,
				DateStart: e.DateStart,
				DateEnd:   e.DateEnd,
				Recurring: e.Recurring,
			}
		}
		if err := encoder.Encode(slimEventFrame{
			Frame:   frame.Frame,
			Partial: frame.Partial,
			Pending: frame.Pending,
			Failed:  frame.Failed,
			Total:   frame.Total,
			Results: slim,
		}); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	})
	if err != nil {
		_ = encoder.Encode(map[string]string{"error": "stream interrupted"})
		c.Writer.Flush()
	}
}

// slimEventFrame is Frame with the event projection in place of full POIs.
type slimEventFrame struct {
	Frame   int               `json:"frame"`
	Partial bool              `json:"partial"`
	Pending []types.Provider  `json:"pending"`
	Failed  []types.Provider  `json:"failed"`
	Total   int               `json:"total"`
	Results []types.SlimEvent `json:"results"`
}

// slimFrame is Frame with the lightweight projection in place of full POIs.
type slimFrame struct {
	Frame   int              `json:"frame"`
	Partial bool             `json:"partial"`
	Pending []types.Provider `json:"pending"`
	Failed  []types.Provider `json:"failed"`
	Total   int              `json:"total"`
	Results []types.SlimPoi  `json:"results"`
}
