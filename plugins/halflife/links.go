package halflife

import (
	"container/list"
	"sync"
)

// maxCacheLinks bounds the cache link index. At about 150 bytes per link,
// 100,000 links take roughly 15 MB. The least recently used link goes first.
const maxCacheLinks = 100_000

// cacheLink ties a semantic cache entry to the report of the answer that
// filled it.
type cacheLink struct {
	cacheID  string
	reportID string
	tenant   string // virtual key ID of the request that filled the entry
}

// cacheLinks is an in-memory LRU index from cache entry ID to report. A cache
// hit serves the answer of an earlier request again, so it resolves to that
// request's report instead of starting a new audit.
type cacheLinks struct {
	mu    sync.Mutex
	max   int
	order *list.List // of *cacheLink, most recently used first
	byID  map[string]*list.Element
}

func newCacheLinks(max int) *cacheLinks {
	return &cacheLinks{max: max, order: list.New(), byID: map[string]*list.Element{}}
}

// add links cacheID to reportID unless it is already linked, and reports
// whether it did: the first answer written to an entry is the one it holds.
func (l *cacheLinks) add(cacheID, reportID, tenant string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byID[cacheID]; ok {
		return false
	}
	l.byID[cacheID] = l.order.PushFront(&cacheLink{cacheID: cacheID, reportID: reportID, tenant: tenant})
	if l.order.Len() > l.max {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.byID, oldest.Value.(*cacheLink).cacheID)
	}
	return true
}

// get returns the link for cacheID and marks it recently used.
func (l *cacheLinks) get(cacheID string) (cacheLink, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.byID[cacheID]
	if !ok {
		return cacheLink{}, false
	}
	l.order.MoveToFront(e)
	return *e.Value.(*cacheLink), true
}

// remove forgets cacheID, for an entry that has been evicted.
func (l *cacheLinks) remove(cacheID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.byID[cacheID]; ok {
		l.order.Remove(e)
		delete(l.byID, cacheID)
	}
}

func (l *cacheLinks) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}
