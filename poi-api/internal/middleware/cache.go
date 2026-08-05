package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/trippier/poi-api/internal/registry"
)

// cacheWriter wraps gin.ResponseWriter to capture the response body.
type cacheWriter struct {
	gin.ResponseWriter
	buf    bytes.Buffer
	status int
}

// Write buffers b into the response body for caching and forwards it to the
// underlying writer, returning the number of bytes written and any error.
func (w *cacheWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

// WriteHeader records status and forwards it to the underlying writer.
func (w *cacheWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Cache returns Gin middleware that caches 200-OK GET responses in Redis,
// keyed by sorted query parameters; a non-empty X-No-Cache request header
// bypasses it, and responses carrying the X-Merge-Partial header are never
// stored (they were assembled before every provider reported). rdb stores
// cached responses and ttl is the entry time-to-live.
func Cache(rdb *redis.Client, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet {
			c.Next()
			return
		}

		if c.GetHeader("X-No-Cache") != "" {
			c.Next()
			return
		}

		// A streamed answer is a sequence of revisions, not a document: the
		// early frames are deliberately incomplete, and storing them would
		// serve a half-finished search to the next caller as if it were the
		// whole thing.
		if isStreamRequest(c) {
			c.Next()
			return
		}

		key := cacheKey(c)
		ctx := c.Request.Context()

		if cached, err := rdb.Get(ctx, key).Bytes(); err == nil {
			c.Header("X-Cache", "HIT")
			c.Data(http.StatusOK, "application/json; charset=utf-8", cached)
			c.Abort()
			return
		}

		cw := &cacheWriter{ResponseWriter: c.Writer, status: http.StatusOK}
		c.Writer = cw
		c.Header("X-Cache", "MISS")

		c.Next()

		if cw.status == http.StatusOK && cw.buf.Len() > 0 && cw.Header().Get("X-Merge-Partial") == "" {
			_ = rdb.Set(ctx, key, cw.buf.Bytes(), ttl).Err()
		}
	}
}

// isStreamRequest reports whether the caller asked for a streamed answer,
// mirroring the search handler's own test.
//
// @param c - The incoming request.
// @returns Whether the answer will be a stream.
func isStreamRequest(c *gin.Context) bool {
	if v := c.Query("stream"); v != "" {
		on, err := strconv.ParseBool(v)
		return err == nil && on
	}
	return strings.Contains(c.GetHeader("Accept"), "application/x-ndjson")
}

// cacheKey derives a deterministic SHA-256 cache key from the request path
// in c, its sorted query parameters, and which BYOK provider keys are
// present, returning the resulting cache key string.
func cacheKey(c *gin.Context) string {
	encoded := c.Request.URL.Query().Encode()
	suffix := byokPresenceSuffix(c)

	h := sha256.Sum256([]byte(c.Request.URL.Path + "?" + encoded + suffix))
	return "poi:cache:" + hex.EncodeToString(h[:])
}

// byokPresenceSuffix returns a sorted ":id" list of BYOK providers whose
// header is present on the request in c (never the header values), as a
// string suffix.
func byokPresenceSuffix(c *gin.Context) string {
	var ids []string
	for id, meta := range registry.All {
		if !meta.Byok || meta.ByokHeader == "" {
			continue
		}
		if c.GetHeader(meta.ByokHeader) != "" {
			ids = append(ids, string(id))
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	return ":" + strings.Join(ids, ":")
}
