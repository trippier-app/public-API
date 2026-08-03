package tilecache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/trippier/poi-api/internal/providers"
	"github.com/trippier/poi-api/pkg/types"
)

// entry is the cached JSON value for one (provider, tile, type, lang) slot;
// an empty Pois slice with non-zero BestRadiusM is the fetched-but-empty sentinel.
type entry struct {
	Pois        []types.RawPoi `json:"pois"`
	BestRadiusM int            `json:"best_radius"`
	FetchedAt   int64          `json:"fetched_at"`
}

// breakerTTL is how long a provider serves straight from cache after a failed
// upstream fetch, bounding a dead upstream to one probe per TTL. The breaker
// write uses a detached context because the request context is precisely what
// just expired.
const breakerTTL = 60 * time.Second

// maxCachedRadiusM is the largest quantized radius served through the tile
// path. Above it the r8 tile cover explodes (a 50 km disk spans ~36k tiles,
// ~250k Redis keys per read) and the MGet/Set storms would stall Redis, so
// wider searches go straight to the provider uncached.
const maxCachedRadiusM = 10000

// defaultCacheTypes is the fallback type set used when the caller doesn't specify q.Types.
var defaultCacheTypes = []types.PoiType{
	types.TypeSee, types.TypeEat, types.TypeDrink,
	types.TypeDo, types.TypeBuy, types.TypeSleep, types.TypeGeneric,
}

// CachedProvider wraps a providers.Provider with an H3-tile Redis cache.
type CachedProvider struct {
	inner providers.Provider
	rdb   *redis.Client
	ttl   time.Duration
	log   *zap.Logger
}

// NewCachedProvider returns a CachedProvider wrapping inner and caching its
// results in rdb, with entries expiring after ttl. log receives cache
// warnings.
func NewCachedProvider(inner providers.Provider, rdb *redis.Client, ttl time.Duration, log *zap.Logger) *CachedProvider {
	return &CachedProvider{inner: inner, rdb: rdb, ttl: ttl, log: log}
}

// Name implements providers.Provider, returning the wrapped provider's
// identifier.
func (c *CachedProvider) Name() types.Provider { return c.inner.Name() }

// SupportsMode implements providers.Provider, reporting whether the wrapped
// provider supports mode, the given search mode.
func (c *CachedProvider) SupportsMode(mode types.SearchMode) bool { return c.inner.SupportsMode(mode) }

// IsByok reports whether the wrapped provider is BYOK (bring-your-own-key),
// returning false if it does not implement providers.ByokProvider.
func (c *CachedProvider) IsByok() bool {
	if bp, ok := c.inner.(providers.ByokProvider); ok {
		return bp.IsByok()
	}
	return false
}

// Search implements providers.Provider using the tile-cache flow; non-radius
// queries bypass the cache. When the upstream fetch fails, cached entries —
// including ones fetched at a coarser radius than requested — are served
// instead of an error, and a short breaker skips the upstream entirely for
// breakerTTL. ctx is the request context and q holds the search query
// parameters. It returns the matching POIs, or an error.
func (c *CachedProvider) Search(ctx context.Context, q types.SearchQuery) ([]types.RawPoi, error) {
	if q.Mode != types.ModeRadius || (q.Lat == 0 && q.Lng == 0) {
		return c.inner.Search(ctx, q)
	}

	effectiveR := Quantize(q.Radius)
	if effectiveR > maxCachedRadiusM {
		return c.inner.Search(ctx, q)
	}
	tiles, err := TileCover(q.Lat, q.Lng, effectiveR)
	if err != nil {
		c.log.Warn("tilecache: cover failed, bypassing cache", zap.Error(err))
		return c.inner.Search(ctx, q)
	}

	poiTypes := q.Types
	if len(poiTypes) == 0 {
		poiTypes = defaultCacheTypes
	}

	providerName := string(c.inner.Name())
	keys, meta := c.buildKeys(providerName, tiles, poiTypes, q.Lang)

	hitPois, coarsePois, missingTiles := c.readCache(ctx, keys, meta, effectiveR)

	if len(missingTiles) == 0 {
		return hitPois, nil
	}

	breakerKey := "poi:tile:breaker:" + providerName
	if c.rdb.Exists(ctx, breakerKey).Val() > 0 {
		return append(hitPois, coarsePois...), nil
	}

	freshPois, _, err := c.fetchMissing(ctx, q, missingTiles, poiTypes)
	if err != nil {
		_ = c.rdb.Set(context.WithoutCancel(ctx), breakerKey, "1", breakerTTL).Err()
		if len(hitPois)+len(coarsePois) > 0 {
			c.log.Warn("tilecache: upstream fetch failed, serving cached data",
				zap.String("provider", providerName), zap.Error(err))
			return append(hitPois, coarsePois...), nil
		}
		return nil, err
	}

	c.writeCache(ctx, providerName, missingTiles, poiTypes, freshPois, effectiveR, q.Lang)

	keptFresh := freshPois[:0]
	for _, p := range freshPois {
		if p.Coords == nil {
			continue
		}
		t := TileOf(p.Coords.Lat, p.Coords.Lng)
		if _, ok := missingTiles[t]; ok {
			keptFresh = append(keptFresh, p)
		}
	}

	return append(hitPois, keptFresh...), nil
}

// keyMeta maps a Redis key to its (tile, type) pair for attributing MGet results.
type keyMeta struct {
	Tile Tile
	Type types.PoiType
}

// buildKeys returns the Redis keys to probe for the given provider, tiles,
// poiTypes and lang (all used to compose each key), alongside the matching
// (tile, type) metadata for each returned key.
func (c *CachedProvider) buildKeys(provider string, tiles []Tile, poiTypes []types.PoiType, lang string) ([]string, []keyMeta) {
	n := len(tiles) * len(poiTypes)
	keys := make([]string, 0, n)
	meta := make([]keyMeta, 0, n)
	for _, t := range tiles {
		hex := TileHex(t)
		for _, pt := range poiTypes {
			keys = append(keys, Key(provider, hex, string(pt), lang))
			meta = append(meta, keyMeta{Tile: t, Type: pt})
		}
	}
	return keys, meta
}

// readCache MGets keys (paired with their (tile, type) metadata in meta)
// using ctx and partitions results into three buckets: POIs precise enough to
// serve as-is, POIs from entries fetched at a coarser radius than effectiveR
// (fallback material only — their tiles are still refetched), and the tiles
// needing a fetch.
func (c *CachedProvider) readCache(ctx context.Context, keys []string, meta []keyMeta, effectiveR int) (hits, coarse []types.RawPoi, missing map[Tile]struct{}) {
	missing = make(map[Tile]struct{})
	if len(keys) == 0 {
		return nil, nil, missing
	}

	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		c.log.Warn("tilecache: MGet failed, treating as miss", zap.Error(err))
		for _, m := range meta {
			missing[m.Tile] = struct{}{}
		}
		return nil, nil, missing
	}

	for i, v := range vals {
		if v == nil {
			missing[meta[i].Tile] = struct{}{}
			continue
		}
		raw, ok := v.(string)
		if !ok {
			missing[meta[i].Tile] = struct{}{}
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			missing[meta[i].Tile] = struct{}{}
			continue
		}
		if e.BestRadiusM > effectiveR {
			missing[meta[i].Tile] = struct{}{}
			coarse = append(coarse, e.Pois...)
			continue
		}
		hits = append(hits, e.Pois...)
	}
	return hits, coarse, missing
}

// fetchMissing runs one inner.Search over the enclosing circle of
// missingTiles, using ctx and query q restricted to poiTypes. It returns the
// fetched POIs, the quantized fetch radius, and an error, if any.
func (c *CachedProvider) fetchMissing(ctx context.Context, q types.SearchQuery, missingTiles map[Tile]struct{}, poiTypes []types.PoiType) ([]types.RawPoi, int, error) {
	missingList := make([]Tile, 0, len(missingTiles))
	for t := range missingTiles {
		missingList = append(missingList, t)
	}

	fetchLat, fetchLng, rawR, err := EnclosingCircle(missingList)
	if err != nil {
		return nil, 0, fmt.Errorf("tilecache: enclosing circle: %w", err)
	}
	fetchR := Quantize(rawR)

	fetchQuery := q
	fetchQuery.Lat = fetchLat
	fetchQuery.Lng = fetchLng
	fetchQuery.Radius = fetchR
	fetchQuery.Types = poiTypes

	pois, err := c.inner.Search(ctx, fetchQuery)
	if err != nil {
		return nil, 0, err
	}
	return pois, fetchR, nil
}

// writeCache pipelines one SET per (tile, type) slot in missingTiles using
// ctx, keyed on provider, tile, type and lang; it stores freshPois bucketed
// by tile and type (empty buckets get a sentinel) alongside bestRadius.
func (c *CachedProvider) writeCache(ctx context.Context, provider string, missingTiles map[Tile]struct{}, poiTypes []types.PoiType, freshPois []types.RawPoi, bestRadius int, lang string) {
	buckets := make(map[Tile]map[types.PoiType][]types.RawPoi, len(missingTiles))
	for _, p := range freshPois {
		if p.Coords == nil {
			continue
		}
		t := TileOf(p.Coords.Lat, p.Coords.Lng)
		if t == 0 {
			continue
		}
		if _, ok := missingTiles[t]; !ok {
			continue
		}
		if buckets[t] == nil {
			buckets[t] = make(map[types.PoiType][]types.RawPoi)
		}
		buckets[t][p.Type] = append(buckets[t][p.Type], p)
	}

	pipe := c.rdb.Pipeline()
	now := time.Now().Unix()
	for t := range missingTiles {
		hex := TileHex(t)
		for _, pt := range poiTypes {
			pois := buckets[t][pt]
			if pois == nil {
				pois = []types.RawPoi{}
			}
			e := entry{Pois: pois, BestRadiusM: bestRadius, FetchedAt: now}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			pipe.Set(ctx, Key(provider, hex, string(pt), lang), data, c.ttl)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		c.log.Warn("tilecache: pipeline exec failed", zap.Error(err))
	}
}
