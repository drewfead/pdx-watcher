package httputil

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2"
)

const defaultLRUMaxEntries = 1000

// CacheTransport is an http.RoundTripper that caches GET responses by request key (Method + URL).
// Cache hits are served from memory; misses are forwarded to Base and cached on success (2xx).
// The cache uses LRU eviction when it reaches MaxEntries. Concurrent requests do not block each other;
// duplicate requests for the same key may both hit the backend.
type CacheTransport struct {
	Base http.RoundTripper

	// MaxEntries is the maximum number of responses to keep in the cache (LRU eviction).
	// Zero means defaultLRUMaxEntries (1000).
	MaxEntries int

	// OnCacheHit, if set, is called for every RoundTrip with the cache key and whether it was a hit.
	// Useful for audit/logging.
	OnCacheHit func(cacheKey string, hit bool)

	initOnce sync.Once
	cache    *lru.Cache[string, *cachedResponse]
	initErr  error
}

type cachedResponse struct {
	Status  int
	Header  http.Header
	Body    []byte
	Expires time.Time // zero = no expiration (honor only LRU)
}

func (t *CacheTransport) ensureCache() error {
	t.initOnce.Do(func() {
		max := t.MaxEntries
		if max <= 0 {
			max = defaultLRUMaxEntries
		}
		t.cache, t.initErr = lru.New[string, *cachedResponse](max)
	})
	return t.initErr
}

// RoundTrip implements http.RoundTripper.
func (t *CacheTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ensureCache(); err != nil {
		return nil, err
	}
	key := req.Method + " " + req.URL.String()
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	// Request Cache-Control: no-cache or max-age=0 bypass cache.
	if requestWantsFresh(req) {
		// fall through to base
	} else if entry, ok := t.cache.Get(key); ok {
		if entry.Expires.IsZero() || time.Now().Before(entry.Expires) {
			if t.OnCacheHit != nil {
				t.OnCacheHit(key, true)
			}
			return t.responseFromCache(req, entry), nil
		}
		t.cache.Remove(key)
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	// Only cache GET with 2xx and when response allows caching.
	if req.Method != http.MethodGet || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if t.OnCacheHit != nil {
			t.OnCacheHit(key, false)
		}
		return resp, nil
	}
	noStore, maxAge := responseCacheControl(resp.Header)
	if noStore {
		if t.OnCacheHit != nil {
			t.OnCacheHit(key, false)
		}
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	entry := &cachedResponse{
		Status:  resp.StatusCode,
		Header:  resp.Header.Clone(),
		Body:    body,
		Expires: cacheExpires(maxAge),
	}
	t.cache.Add(key, entry)
	if t.OnCacheHit != nil {
		t.OnCacheHit(key, false)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

func (t *CacheTransport) responseFromCache(req *http.Request, entry *cachedResponse) *http.Response {
	return &http.Response{
		Status:        http.StatusText(entry.Status),
		StatusCode:    entry.Status,
		Header:        entry.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(entry.Body)),
		ContentLength: int64(len(entry.Body)),
		Request:       req,
	}
}

// requestWantsFresh returns true if the request's Cache-Control asks to bypass cache (no-cache or max-age=0).
func requestWantsFresh(req *http.Request) bool {
	cc := req.Header.Get("Cache-Control")
	if cc == "" {
		return false
	}
	for part := range strings.SplitSeq(cc, ",") {
		part = strings.TrimSpace(part)
		if part == "no-cache" {
			return true
		}
		if after, ok := strings.CutPrefix(part, "max-age="); ok {
			val := strings.TrimSpace(after)
			if n, err := strconv.Atoi(val); err == nil && n <= 0 {
				return true
			}
		}
	}
	return false
}

// responseCacheControl parses Cache-Control from response headers.
// Returns noStore (do not cache) and maxAge in seconds (0 = not set or no caching directive).
func responseCacheControl(header http.Header) (noStore bool, maxAge int) {
	// Multiple Cache-Control headers are concatenated with comma per RFC 7230.
	for _, cc := range header["Cache-Control"] {
		for part := range strings.SplitSeq(cc, ",") {
			part = strings.TrimSpace(strings.ToLower(part))
			if part == "no-store" || part == "no-cache" {
				noStore = true
			}
			if strings.HasPrefix(part, "max-age=") {
				val := strings.TrimSpace(part[8:])
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					maxAge = n
				}
			}
			if strings.HasPrefix(part, "s-maxage=") {
				val := strings.TrimSpace(part[9:])
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					maxAge = n
				}
			}
		}
	}
	return noStore, maxAge
}

func cacheExpires(maxAgeSeconds int) time.Time {
	if maxAgeSeconds <= 0 {
		return time.Time{}
	}
	return time.Now().Add(time.Duration(maxAgeSeconds) * time.Second)
}
