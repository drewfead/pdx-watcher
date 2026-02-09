package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// PageStableTimeout is the timeout used when waiting for page stability or running eval scripts.
var PageStableTimeout = 30 * time.Second

// Interface runs a callback with a rod page loaded at a given URL and can fetch JSON with optional caching.
// Implementations may reuse a single browser process (e.g. headlessBrowser).
type Interface interface {
	WithPage(ctx context.Context, url string, fn func(*rod.Page) error) error
	// FetchJSON returns a callback that fetches url in the page and unmarshals into dest. Uses internal cache on hit.
	// Use with WithPage: b.WithPage(ctx, baseURL, b.FetchJSON(url, &obj)).
	FetchJSON(ctx context.Context, url string, dest any) func(*rod.Page) error

	io.Closer
}

// headlessBrowser manages a single rod browser instance. A channel of capacity 1 serializes
// access: callers receive the browser, use it, then send it back so only one WithPage runs at a time.
// Cache holds url -> JSON string for FetchJSON to avoid re-fetching.
type headlessBrowser struct {
	initOnce sync.Once
	initErr  error
	ch       chan *rod.Browser
	cache    map[string]string
	cacheMu  sync.Mutex
}

// Headless returns a Browser that lazily launches one headless chrome browser and reuses it.
func Headless() Interface {
	h := &headlessBrowser{
		ch:    make(chan *rod.Browser, 1),
		cache: make(map[string]string),
	}
	h.initOnce.Do(func() {
		u, err := launcher.New().Logger(newRodLauncherLogger()).Leakless(false).Launch()
		if err != nil {
			h.initErr = fmt.Errorf("launch browser: %w", err)
			close(h.ch)
			return
		}
		browser := rod.New().ControlURL(u)
		if err := browser.Connect(); err != nil {
			h.initErr = fmt.Errorf("connect to browser: %w", err)
			close(h.ch)
			return
		}
		h.ch <- browser
	})
	return h
}

func (h *headlessBrowser) Close() error {
	browser, ok := <-h.ch
	if !ok {
		return h.initErr
	}
	return browser.Close()
}

// WithPage receives the shared browser from the channel, creates a page at url, runs fn, then sends the browser back.
// Serializes with other callers (one WithPage at a time). The page is closed when fn returns.
func (h *headlessBrowser) WithPage(ctx context.Context, url string, fn func(page *rod.Page) error) error {
	if h.initErr != nil {
		return h.initErr
	}
	browser, ok := <-h.ch
	if !ok {
		return h.initErr
	}
	defer func() { h.ch <- browser }()

	page, err := browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return fmt.Errorf("create page: %w", err)
	}
	defer page.MustClose()

	page = page.Context(ctx)

	if err := page.Navigate(url); err != nil {
		return fmt.Errorf("navigate to %s: %w", url, err)
	}
	if err := rod.Try(func() {
		page.Timeout(PageStableTimeout).MustWaitStable()
	}); err != nil {
		return fmt.Errorf("wait for page stable: %w", err)
	}

	return fn(page)
}

// FetchJSON returns a callback that fetches url in the page and unmarshals into dest. Uses internal cache on hit.
func (h *headlessBrowser) FetchJSON(ctx context.Context, urlStr string, dest any) func(*rod.Page) error {
	return func(page *rod.Page) error {
		h.cacheMu.Lock()
		raw, ok := h.cache[urlStr]
		h.cacheMu.Unlock()
		if ok {
			return json.Unmarshal([]byte(raw), dest)
		}
		result, err := page.Context(ctx).Timeout(PageStableTimeout).Eval(fetchJSONScript, urlStr)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", urlStr, err)
		}
		raw = result.Value.Str()
		h.cacheMu.Lock()
		h.cache[urlStr] = raw
		h.cacheMu.Unlock()
		return json.Unmarshal([]byte(raw), dest)
	}
}

// fetchJSONScript fetches url in the page context and returns the response body as JSON string.
const fetchJSONScript = `(url) => {
	return fetch(url).then(r => {
		if (!r.ok) throw new Error('HTTP ' + r.status);
		return r.json();
	}).then(obj => JSON.stringify(obj));
}`

// rodLauncherLogger is an io.Writer that forwards launcher output (e.g. download progress) to slog at debug level.
type rodLauncherLogger struct {
	buf []byte
}

func (w *rodLauncherLogger) Write(p []byte) (n int, err error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" {
			slog.Debug("rod launcher", "message", line)
		}
	}
	return len(p), nil
}

func newRodLauncherLogger() io.Writer {
	return &rodLauncherLogger{}
}
