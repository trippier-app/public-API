package tilecache_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/trippier/poi-api/internal/providers/overpass"
	"github.com/trippier/poi-api/internal/tilecache"
	"github.com/trippier/poi-api/pkg/types"
)

// fakeOverpassResponse mirrors the real Overpass API JSON for a handful of
// Paris POIs. The exact coords matter: each sits in a distinct r8 cell so the
// per-tile dispatch step has multiple buckets to exercise.
const fakeOverpassResponse = `{
  "elements": [
    {"type":"node","id":1,"lat":48.8566,"lon":2.3522,"tags":{"name":"Notre-Dame","tourism":"attraction"}},
    {"type":"node","id":2,"lat":48.8606,"lon":2.3376,"tags":{"name":"Louvre","tourism":"museum"}},
    {"type":"node","id":3,"lat":48.8530,"lon":2.3499,"tags":{"name":"Sainte-Chapelle","tourism":"attraction"}},
    {"type":"node","id":4,"lat":48.8584,"lon":2.2945,"tags":{"name":"Tour Eiffel","tourism":"attraction"}},
    {"type":"node","id":5,"lat":48.8638,"lon":2.3491,"tags":{"name":"Hôtel de Ville","amenity":"restaurant"}}
  ]
}`

// newFakeOverpass returns a httptest server that counts inbound calls and
// always returns fakeOverpassResponse. The counter lets each test assert how
// many times the upstream API was actually hit through the cache wrapper.
func newFakeOverpass(counter *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeOverpassResponse))
	}))
}

// newIntegrationHarness wires a real overpass.Provider against a fake upstream,
// then wraps it with CachedProvider over miniredis. Returns the wrapper, the
// per-call counter, and a teardown.
func newIntegrationHarness(t *testing.T) (*tilecache.CachedProvider, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := newFakeOverpass(&calls)
	t.Cleanup(srv.Close)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })

	inner := overpass.NewWithURL(srv.URL)
	cp := tilecache.NewCachedProvider(inner, rdb, time.Hour, zap.NewNop())
	return cp, &calls
}

func TestIntegration_ZoomPanScenario(t *testing.T) {
	cp, calls := newIntegrationHarness(t)
	ctx := context.Background()

	q1 := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 1000,
		Types: []types.PoiType{types.TypeSee, types.TypeEat},
	}
	if _, err := cp.Search(ctx, q1); err != nil {
		t.Fatalf("q1: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("after q1 (cold): want 1 upstream call, got %d", got)
	}

	if _, err := cp.Search(ctx, q1); err != nil {
		t.Fatalf("q2: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("after q2 (identical): want 1 upstream call, got %d", got)
	}

	q3 := q1
	q3.Radius = 950
	if _, err := cp.Search(ctx, q3); err != nil {
		t.Fatalf("q3: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("after q3 (radius ε): want 1 upstream call, got %d", got)
	}

	q4 := q1
	q4.Lng = 2.3590
	if _, err := cp.Search(ctx, q4); err != nil {
		t.Fatalf("q4: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("after q4 (pan east): want 2 upstream calls, got %d", got)
	}

	q5 := q1
	q5.Radius = 500
	if _, err := cp.Search(ctx, q5); err != nil {
		t.Fatalf("q5: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("after q5 (zoom-in to 500m): want 3 upstream calls, got %d", got)
	}

	if _, err := cp.Search(ctx, q1); err != nil {
		t.Fatalf("q6: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("after q6 (back to q1): want 3 upstream calls, got %d", got)
	}
}

func TestIntegration_TileDispatchKeepsPoisLocal(t *testing.T) {
	cp, _ := newIntegrationHarness(t)
	ctx := context.Background()

	primer := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 5000,
		Types: []types.PoiType{types.TypeSee, types.TypeEat},
	}
	if _, err := cp.Search(ctx, primer); err != nil {
		t.Fatal(err)
	}

	eiffel := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8584, Lng: 2.2945, Radius: 500,
		Types: []types.PoiType{types.TypeSee},
	}
	got, err := cp.Search(ctx, eiffel)
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range got {
		if p.Name == "Notre-Dame" || p.Name == "Sainte-Chapelle" || p.Name == "Hôtel de Ville" {
			t.Errorf("Eiffel-area query leaked distant POI %q", p.Name)
		}
	}
}

func TestIntegration_RepeatedJitteredQueries(t *testing.T) {
	cp, calls := newIntegrationHarness(t)
	ctx := context.Background()

	base := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 1000,
		Types: []types.PoiType{types.TypeSee},
	}
	for i := 0; i < 50; i++ {
		q := base
		q.Lat += float64(i%5-2) * 0.0002
		q.Lng += float64(i%5-2) * 0.0002
		if _, err := cp.Search(ctx, q); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	if got := calls.Load(); got > 10 {
		t.Errorf("expected few upstream calls under jitter, got %d (50 queries)", got)
	}
	t.Logf("50 jittered queries → %d upstream calls", calls.Load())
}
