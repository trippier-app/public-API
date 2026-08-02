// Package tilecache provides an H3-tile-based Redis cache wrapper for POI
// providers, recording per slot the smallest fetch radius that populated each
// tile so it can judge whether cached data is precise enough for a query.
package tilecache

import (
	"fmt"
	"math"

	"github.com/uber/h3-go/v4"
)

// Resolution is the fixed H3 resolution used for all cache tiles (r8, ~461m edge, ~0.74km²).
const Resolution = 8

// edgeMeters is the average H3 r8 hexagon edge length in meters.
const edgeMeters = 461.354684

// RadiusTiers lists the canonical fetch radii in meters; Quantize rounds any
// incoming radius up to the nearest tier so near-identical queries share a
// cache slot.
var RadiusTiers = []int{500, 1000, 2000, 5000, 10000, 25000, 50000}

// Tile is an opaque identifier for one H3 r8 hexagon.
type Tile = h3.Cell

// Quantize rounds a radius r (in meters) up to the nearest cache tier and
// returns that tier, capped at the maximum tier.
func Quantize(r int) int {
	for _, tier := range RadiusTiers {
		if r <= tier {
			return tier
		}
	}
	return RadiusTiers[len(RadiusTiers)-1]
}

// LatLngToTile resolves the H3 r8 cell containing the coordinate given by
// lat and lng (in degrees), returning the containing tile or an error.
func LatLngToTile(lat, lng float64) (Tile, error) {
	return h3.LatLngToCell(h3.NewLatLng(lat, lng), Resolution)
}

// TileCenter computes the geographic centre of tile t, returning its
// latitude and longitude, or a non-nil err if the lookup fails.
func TileCenter(t Tile) (lat, lng float64, err error) {
	ll, err := h3.CellToLatLng(t)
	if err != nil {
		return 0, 0, err
	}
	return ll.Lat, ll.Lng, nil
}

// TileCover returns the H3 r8 tiles intersecting a disk of radius radiusM
// (meters) centred at (lat, lng) in degrees, or an error. It is conservative:
// it never misses an intersecting tile but may include boundary extras.
func TileCover(lat, lng float64, radiusM int) ([]Tile, error) {
	origin, err := LatLngToTile(lat, lng)
	if err != nil {
		return nil, fmt.Errorf("tilecache: origin cell: %w", err)
	}
	k := int(math.Ceil(float64(radiusM)/edgeMeters)) + 1
	if k < 1 {
		k = 1
	}
	cells, err := h3.GridDisk(origin, k)
	if err != nil {
		return nil, fmt.Errorf("tilecache: grid disk: %w", err)
	}
	return cells, nil
}

// EnclosingCircle computes an approximate (not optimal) circle over tile
// centroids that contains every tile in tiles. It returns the centre
// (centerLat, centerLng), a covering radiusM, and an err if tiles is empty.
func EnclosingCircle(tiles []Tile) (centerLat, centerLng float64, radiusM int, err error) {
	if len(tiles) == 0 {
		return 0, 0, 0, fmt.Errorf("tilecache: enclosing circle on empty tile set")
	}
	if len(tiles) == 1 {
		lat, lng, err := TileCenter(tiles[0])
		if err != nil {
			return 0, 0, 0, err
		}
		return lat, lng, int(math.Ceil(edgeMeters)), nil
	}

	var sumLat, sumLng float64
	centers := make([]h3.LatLng, len(tiles))
	for i, t := range tiles {
		lat, lng, err := TileCenter(t)
		if err != nil {
			return 0, 0, 0, err
		}
		centers[i] = h3.NewLatLng(lat, lng)
		sumLat += lat
		sumLng += lng
	}
	cLat := sumLat / float64(len(tiles))
	cLng := sumLng / float64(len(tiles))
	cLL := h3.NewLatLng(cLat, cLng)

	maxDist := 0.0
	for _, ll := range centers {
		if d := h3.GreatCircleDistanceM(cLL, ll); d > maxDist {
			maxDist = d
		}
	}
	return cLat, cLng, int(math.Ceil(maxDist + edgeMeters)), nil
}

// TileOf attributes the coordinate (lat, lng), in degrees, to its H3 r8
// tile bucket, returning the tile or the zero Tile on error.
func TileOf(lat, lng float64) Tile {
	c, err := LatLngToTile(lat, lng)
	if err != nil {
		return 0
	}
	return c
}

// Key builds the Redis key for one (provider, tileHex, poiType, lang) slot.
// The tile is serialised as its hex H3 index so keys stay inspectable in
// redis-cli. It returns the composed key.
func Key(provider, tileHex, poiType, lang string) string {
	return fmt.Sprintf("poi:tile:%s:%s:%s:%s", provider, tileHex, poiType, lang)
}

// TileHex renders tile t as its hex H3 index string, used in cache keys,
// and returns that hex string representation.
func TileHex(t Tile) string {
	return h3.CellToString(t)
}
