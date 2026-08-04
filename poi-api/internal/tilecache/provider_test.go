package tilecache_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/trippier/poi-api/internal/tilecache"
	"github.com/trippier/poi-api/pkg/types"
)

// mockProvider records every Search call and returns a fixed set of POIs
// scattered around the query centre. Used to assert when the cache wrapper
// hits the upstream vs serves from Redis.
type mockProvider struct {
	name     types.Provider
	callCnt  atomic.Int32
	lastQ    types.SearchQuery
	response []types.RawPoi
	err      error
}

func (m *mockProvider) Name() types.Provider               { return m.name }
func (m *mockProvider) SupportsMode(types.SearchMode) bool { return true }
func (m *mockProvider) Search(_ context.Context, q types.SearchQuery) ([]types.RawPoi, error) {
	m.callCnt.Add(1)
	m.lastQ = q
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func newMock(pois []types.RawPoi) *mockProvider {
	return &mockProvider{name: "mock", response: pois}
}

// newCacheHarness wires miniredis + the wrapper for one test.
func newCacheHarness(t *testing.T, m *mockProvider) (*tilecache.CachedProvider, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cp := tilecache.NewCachedProvider(m, rdb, time.Hour, zap.NewNop())
	return cp, mr
}

// makePoi returns a RawPoi at the given coordinates with the given type.
func makePoi(id string, lat, lng float64, pt types.PoiType) types.RawPoi {
	return types.RawPoi{
		ID:       id,
		Name:     id,
		Type:     pt,
		Coords:   &types.Coordinates{Lat: lat, Lng: lng},
		Provider: "mock",
	}
}

func TestCachedProvider_ColdCache_HitsUpstreamOnce(t *testing.T) {
	pois := []types.RawPoi{
		makePoi("p1", 48.8566, 2.3522, types.TypeSee),
		makePoi("p2", 48.8576, 2.3532, types.TypeEat),
	}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	q := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 1000,
		Types: []types.PoiType{types.TypeSee, types.TypeEat},
	}
	got, err := cp.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 pois, got %d", len(got))
	}
	if m.callCnt.Load() != 1 {
		t.Errorf("expected 1 upstream call, got %d", m.callCnt.Load())
	}
}

func TestCachedProvider_WarmCache_NoUpstream(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	q := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 1000,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if m.callCnt.Load() != 1 {
		t.Errorf("expected 1 upstream call (second served from cache), got %d", m.callCnt.Load())
	}
}

func TestCachedProvider_RadiusEpsilonSameTier_NoRefetch(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	base := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 900,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	for _, r := range []int{950, 1000, 800} {
		q := base
		q.Radius = r
		if _, err := cp.Search(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if m.callCnt.Load() != 1 {
		t.Errorf("expected 1 upstream call across same-tier variations, got %d", m.callCnt.Load())
	}
}

func TestCachedProvider_ZoomToFinerTier_Refetches(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	q1 := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 2000,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), q1); err != nil {
		t.Fatal(err)
	}
	q2 := q1
	q2.Radius = 500
	if _, err := cp.Search(context.Background(), q2); err != nil {
		t.Fatal(err)
	}
	if m.callCnt.Load() != 2 {
		t.Errorf("expected 2 upstream calls (zoom-in is a miss), got %d", m.callCnt.Load())
	}
}

func TestCachedProvider_ZoomOutWithinCovered_NoRefetch(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	q1 := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 500,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), q1); err != nil {
		t.Fatal(err)
	}
	callsAfter := m.callCnt.Load()
	q2 := q1
	q2.Radius = 1000
	if _, err := cp.Search(context.Background(), q2); err != nil {
		t.Fatal(err)
	}
	if m.callCnt.Load()-callsAfter > 1 {
		t.Errorf("expected at most 1 extra upstream call for the ring, got %d", m.callCnt.Load()-callsAfter)
	}
}

func TestCachedProvider_EmptyResult_SentinelPreventsRefetch(t *testing.T) {
	m := newMock(nil)
	cp, _ := newCacheHarness(t, m)

	q := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 500,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if m.callCnt.Load() != 1 {
		t.Errorf("expected 1 upstream call (empty sentinel served), got %d", m.callCnt.Load())
	}
}

func TestCachedProvider_NonRadiusMode_Bypasses(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	q := types.SearchQuery{Mode: types.ModeDistrict, District: "Paris"}
	for i := 0; i < 3; i++ {
		if _, err := cp.Search(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if m.callCnt.Load() != 3 {
		t.Errorf("expected 3 upstream calls (cache bypassed for district mode), got %d", m.callCnt.Load())
	}
}

func TestCachedProvider_OverlappingPan_FetchCenterShifts(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	q1 := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 1000,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), q1); err != nil {
		t.Fatal(err)
	}
	firstFetchCenterLat := m.lastQ.Lat
	firstFetchCenterLng := m.lastQ.Lng

	q2 := q1
	q2.Lng = 2.3590
	if _, err := cp.Search(context.Background(), q2); err != nil {
		t.Fatal(err)
	}
	if m.callCnt.Load() != 2 {
		t.Errorf("expected 2 upstream calls, got %d", m.callCnt.Load())
	}
	if m.lastQ.Lng <= firstFetchCenterLng {
		t.Errorf("expected fetch centre to shift east after pan, got %f (first %f)", m.lastQ.Lng, firstFetchCenterLng)
	}
	_ = firstFetchCenterLat
}

func TestCachedProvider_FetchError_ServesCoarseFallback(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, _ := newCacheHarness(t, m)

	warm := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 5000,
		Types: []types.PoiType{types.TypeSee},
	}
	if _, err := cp.Search(context.Background(), warm); err != nil {
		t.Fatal(err)
	}

	m.err = context.DeadlineExceeded
	fine := warm
	fine.Radius = 500
	got, err := cp.Search(context.Background(), fine)
	if err != nil {
		t.Fatalf("expected coarse fallback, got error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Errorf("expected the cached coarse POI back, got %v", got)
	}
}

func TestCachedProvider_FetchError_TripsBreaker(t *testing.T) {
	m := newMock(nil)
	m.err = context.DeadlineExceeded
	cp, _ := newCacheHarness(t, m)

	q := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 1000,
		Types: []types.PoiType{types.TypeSee},
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cp.Search(cancelled, q); err == nil {
		t.Fatal("expected the first search to surface the upstream error")
	}
	if m.callCnt.Load() != 1 {
		t.Fatalf("expected 1 upstream call, got %d", m.callCnt.Load())
	}

	// Under the breaker with nothing cached, the provider must fail rather
	// than pass an empty answer off as complete — the merge would not flag
	// it partial and the HTTP cache would store the amputated response.
	if _, err := cp.Search(context.Background(), q); err == nil {
		t.Fatal("expected an error under breaker when nothing is cached")
	}
	if m.callCnt.Load() != 1 {
		t.Errorf("expected the breaker to skip the upstream, got %d calls", m.callCnt.Load())
	}
}

func TestCachedProvider_HugeRadius_BypassesTilePath(t *testing.T) {
	pois := []types.RawPoi{makePoi("p1", 48.8566, 2.3522, types.TypeSee)}
	m := newMock(pois)
	cp, mr := newCacheHarness(t, m)

	q := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 30000,
		Types: []types.PoiType{types.TypeSee},
	}
	for i := 0; i < 2; i++ {
		if _, err := cp.Search(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if m.callCnt.Load() != 2 {
		t.Errorf("expected 2 direct upstream calls above the radius cap, got %d", m.callCnt.Load())
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Errorf("expected no tile keys written above the radius cap, got %d", len(keys))
	}
}

func TestCachedProvider_CappedFetch_NoFalseEmptyBeyondHorizon(t *testing.T) {
	clustered := make([]types.RawPoi, 100)
	for i := range clustered {
		clustered[i] = makePoi(
			"p"+string(rune('a'+i%26))+string(rune('0'+i/26)),
			48.8566+float64(i)*0.0001, 2.3522, types.TypeSee)
	}
	m := newMock(clustered)
	cp, _ := newCacheHarness(t, m)

	wide := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 5000,
		Types: []types.PoiType{types.TypeSee}, Limit: 30,
	}
	if _, err := cp.Search(context.Background(), wide); err != nil {
		t.Fatal(err)
	}
	callsAfterWide := m.callCnt.Load()

	edge := wide
	edge.Lat = 48.9000
	if _, err := cp.Search(context.Background(), edge); err != nil {
		t.Fatal(err)
	}
	if m.callCnt.Load() == callsAfterWide {
		t.Error("expected a refetch at the edge: a capped response must not sentinel tiles beyond its farthest POI")
	}
}

type scriptedProvider struct {
	name      types.Provider
	callCnt   atomic.Int32
	responses [][]types.RawPoi
}

func (s *scriptedProvider) Name() types.Provider               { return s.name }
func (s *scriptedProvider) SupportsMode(types.SearchMode) bool { return true }
func (s *scriptedProvider) Search(_ context.Context, _ types.SearchQuery) ([]types.RawPoi, error) {
	i := int(s.callCnt.Add(1)) - 1
	if i >= len(s.responses) {
		i = len(s.responses) - 1
	}
	return s.responses[i], nil
}

func TestCachedProvider_TruncatedFetch_KeepsFarTilesMissing(t *testing.T) {
	cluster := make([]types.RawPoi, 100)
	for i := range cluster {
		cluster[i] = makePoi(fmt.Sprintf("a%d", i), 48.8566+float64(i%10)*0.0002, 2.3522, types.TypeSee)
	}
	band := []types.RawPoi{makePoi("b1", 48.8566, 2.4100, types.TypeSee)}
	p := &scriptedProvider{name: "mock", responses: [][]types.RawPoi{cluster, band}}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cp := tilecache.NewCachedProvider(p, rdb, time.Hour, zap.NewNop())

	wide := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 5000,
		Types: []types.PoiType{types.TypeSee}, Limit: 30,
	}
	if _, err := cp.Search(context.Background(), wide); err != nil {
		t.Fatal(err)
	}

	overlap := wide
	overlap.Lng = 2.4100
	got, err := cp.Search(context.Background(), overlap)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, poi := range got {
		if poi.ID == "b1" {
			found = true
		}
	}
	if !found {
		t.Error("expected the band POI: a truncated response must not seal far tiles as empty")
	}
}

func TestCachedProvider_SparseFetch_ProvisionalEmptyDecays(t *testing.T) {
	center := []types.RawPoi{makePoi("a1", 48.8566, 2.3522, types.TypeSee)}
	band := []types.RawPoi{makePoi("b1", 48.8566, 2.4100, types.TypeSee)}
	p := &scriptedProvider{name: "mock", responses: [][]types.RawPoi{center, band}}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cp := tilecache.NewCachedProvider(p, rdb, time.Hour, zap.NewNop())

	wide := types.SearchQuery{
		Mode: types.ModeRadius, Lat: 48.8566, Lng: 2.3522, Radius: 5000,
		Types: []types.PoiType{types.TypeSee}, Limit: 30,
	}
	if _, err := cp.Search(context.Background(), wide); err != nil {
		t.Fatal(err)
	}

	overlap := wide
	overlap.Lng = 2.4100
	got, err := cp.Search(context.Background(), overlap)
	if err != nil {
		t.Fatal(err)
	}
	for _, poi := range got {
		if poi.ID == "b1" {
			t.Fatal("expected the provisional empty to hold within its TTL")
		}
	}

	mr.FastForward(3 * time.Minute)
	got, err = cp.Search(context.Background(), overlap)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, poi := range got {
		if poi.ID == "b1" {
			found = true
		}
	}
	if !found {
		t.Error("expected the band POI once the provisional empty lapsed")
	}
}
