package scraper

import (
	"container/heap"
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/drewfead/pdx-watcher/internal"
)

// Interleaved runs multiple scrapers in parallel and merges their results
// into a single stream ordered by showtime start time.
// Each scraper is assumed to return events in timestamp order; a k-way merge
// is used so results can be streamed without buffering everything in memory.
func Interleaved(scrapers ...internal.Scraper) internal.Scraper {
	if len(scrapers) == 0 {
		return None()
	}
	// Flatten any nested interleaved scrapers so descriptor and behavior are simple
	var flat []internal.Scraper
	for _, s := range scrapers {
		if s == nil {
			continue
		}
		if i, ok := s.(*interleavedScraper); ok {
			flat = append(flat, i.scrapers...)
		} else {
			flat = append(flat, s)
		}
	}
	if len(flat) == 0 {
		return None()
	}
	if len(flat) == 1 {
		return flat[0]
	}
	return &interleavedScraper{scrapers: flat}
}

type interleavedScraper struct {
	scrapers []internal.Scraper
}

func (s *interleavedScraper) Descriptor() string {
	parts := make([]string, 0, len(s.scrapers))
	for _, sc := range s.scrapers {
		parts = append(parts, sc.Descriptor())
	}
	return "interleaved:" + strings.Join(parts, ",")
}

func (s *interleavedScraper) ScrapeShowtimes(
	ctx context.Context,
	req internal.ListShowtimesRequest,
) (<-chan internal.ShowtimeListItem, error) {
	out := make(chan internal.ShowtimeListItem)
	go func() {
		defer close(out)
		s.run(ctx, req, out)
	}()
	return out, nil
}

// mergedSlot is sent from each scraper goroutine to the merge loop.
// Item is nil when that scraper's stream has closed.
type mergedSlot struct {
	item  *internal.ShowtimeListItem
	index int // scraper index
}

func (s *interleavedScraper) run(
	ctx context.Context,
	req internal.ListShowtimesRequest,
	out chan<- internal.ShowtimeListItem,
) {
	n := len(s.scrapers)
	mergeChan := make(chan mergedSlot)
	var wg sync.WaitGroup
	wg.Add(n)
	for i, sc := range s.scrapers {
		sc := sc
		i := i
		go func() {
			defer wg.Done()
			ch, err := sc.ScrapeShowtimes(ctx, req)
			if err != nil {
				slog.Warn("interleaved: scraper failed", "descriptor", sc.Descriptor(), "error", err)
				mergeChan <- mergedSlot{index: i} // signal stream exhausted
				return
			}
			for it := range ch {
				select {
				case mergeChan <- mergedSlot{item: &it, index: i}:
				case <-ctx.Done():
					return
				}
			}
			mergeChan <- mergedSlot{index: i} // signal stream exhausted
		}()
	}
	go func() {
		wg.Wait()
		close(mergeChan)
	}()

	// K-way merge: one slot per scraper (current head), heap ordered by StartTime.
	// Each stream may send multiple items before we pop; we only keep the current head in buffer,
	// and queue the rest in pending[i] until we pop that stream.
	buffer := make([]*internal.ShowtimeListItem, n)
	pending := make([][]*internal.ShowtimeListItem, n)
	closed := make([]bool, n)
	h := &mergeHeap{buffer: buffer}
	heap.Init(h)
	limit := req.Limit
	if limit <= 0 {
		limit = 1<<31 - 1
	}
	sent := 0
	canPop := func() bool {
		for i := range n {
			if !closed[i] && buffer[i] == nil && len(pending[i]) == 0 {
				return false
			}
		}
		return true
	}
	refill := func(j int) {
		if len(pending[j]) > 0 {
			buffer[j] = pending[j][0]
			pending[j] = pending[j][1:]
			heap.Push(h, j)
		}
	}
	for {
		if canPop() && h.Len() > 0 && sent < limit {
			j := heap.Pop(h).(int)
			item := buffer[j]
			buffer[j] = nil
			refill(j)
			if item == nil {
				continue
			}
			select {
			case out <- *item:
				sent++
			case <-ctx.Done():
				return
			}
			continue
		}
		slot, ok := <-mergeChan
		if !ok {
			for h.Len() > 0 && sent < limit {
				j := heap.Pop(h).(int)
				item := buffer[j]
				buffer[j] = nil
				refill(j)
				if item != nil {
					select {
					case out <- *item:
						sent++
					case <-ctx.Done():
						return
					}
				}
			}
			return
		}
		if slot.item == nil {
			closed[slot.index] = true
			continue
		}
		if buffer[slot.index] == nil {
			buffer[slot.index] = slot.item
			heap.Push(h, slot.index)
		} else {
			pending[slot.index] = append(pending[slot.index], slot.item)
		}
	}
}

// mergeHeap is a min-heap of scraper indices ordered by buffer[i].Showtime.StartTime.
type mergeHeap struct {
	indices []int
	buffer  []*internal.ShowtimeListItem
}

func (h *mergeHeap) Len() int {
	return len(h.indices)
}

func (h *mergeHeap) Less(i, j int) bool {
	return h.buffer[h.indices[i]].Showtime.StartTime.Before(h.buffer[h.indices[j]].Showtime.StartTime)
}

func (h *mergeHeap) Swap(i, j int) {
	h.indices[i], h.indices[j] = h.indices[j], h.indices[i]
}

func (h *mergeHeap) Push(x any) {
	h.indices = append(h.indices, x.(int))
}

func (h *mergeHeap) Pop() any {
	n := len(h.indices) - 1
	out := h.indices[n]
	h.indices = h.indices[:n]
	return out
}
