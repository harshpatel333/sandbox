package proxy

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	ownershipCacheTTL   = time.Second
	ownershipCacheSize  = 4096
	ownershipMaxLookups = 32
	// Callers held for a slot are bounded too, so a backlog or random IDs still
	// shed load; 256 is several rounds of lookups within one lookup timeout.
	ownershipMaxWaiters = 256
)

var errOwnershipCapacity = errors.New("ownership lookup capacity exhausted")

type ownershipEntry struct {
	id      string
	route   SandboxRoute
	err     error
	expires time.Time
}

type ownershipFlight struct {
	done  chan struct{}
	route SandboxRoute
	err   error
}

// CachedOwnershipResolver permits at most one second of ownership staleness.
// Expiry starts before the query; failures never extend a previously valid route.
// One canceled HTTP request must not cancel a lookup shared by other requests.
type CachedOwnershipResolver struct {
	ctx     context.Context
	source  OwnershipResolver
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
	flights map[string]*ownershipFlight
	// freed is closed when a lookup finishes; created only while callers wait.
	freed   chan struct{}
	waiters int
	now     func() time.Time
}

func NewCachedOwnershipResolver(ctx context.Context, source OwnershipResolver) *CachedOwnershipResolver {
	return &CachedOwnershipResolver{ctx: ctx, source: source, entries: make(map[string]*list.Element), flights: make(map[string]*ownershipFlight), now: time.Now}
}

func (c *CachedOwnershipResolver) ResolveSandbox(ctx context.Context, id string) (SandboxRoute, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return SandboxRoute{}, err
	}
	id = parsed.String()
	if err := ctx.Err(); err != nil {
		return SandboxRoute{}, err
	}
	// A full lookup table waits for a slot rather than failing a burst; one
	// lookup timeout bounds the wait, since a slot frees within it.
	var waitLimit *time.Timer
	var f *ownershipFlight
	for {
		c.mu.Lock()
		if e := c.entries[id]; e != nil {
			entry := e.Value.(ownershipEntry)
			if c.now().Before(entry.expires) {
				c.lru.MoveToFront(e)
				c.mu.Unlock()
				return entry.route, entry.err
			}
			c.lru.Remove(e)
			delete(c.entries, id)
		}
		f = c.flights[id]
		if f != nil || len(c.flights) < ownershipMaxLookups {
			break
		}
		if c.waiters >= ownershipMaxWaiters {
			c.mu.Unlock()
			if waitLimit != nil {
				waitLimit.Stop()
			}
			return SandboxRoute{}, errOwnershipCapacity
		}
		if c.freed == nil {
			c.freed = make(chan struct{})
		}
		freed := c.freed
		c.waiters++
		c.mu.Unlock()
		if waitLimit == nil {
			waitLimit = time.NewTimer(ownershipLookupTimeout)
		}
		var waitErr error
		select {
		case <-freed:
		case <-waitLimit.C:
			waitErr = errOwnershipCapacity
		case <-ctx.Done():
			waitErr = ctx.Err()
		case <-c.ctx.Done():
			waitErr = c.ctx.Err()
		}
		c.mu.Lock()
		c.waiters--
		c.mu.Unlock()
		if waitErr != nil {
			waitLimit.Stop()
			return SandboxRoute{}, waitErr
		}
	}
	if waitLimit != nil {
		waitLimit.Stop()
	}
	if f == nil {
		f = &ownershipFlight{done: make(chan struct{})}
		c.flights[id] = f
		started := c.now()
		go c.lookup(id, f, started)
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return SandboxRoute{}, ctx.Err()
	case <-c.ctx.Done():
		return SandboxRoute{}, c.ctx.Err()
	case <-f.done:
		return f.route, f.err
	}
}

func (c *CachedOwnershipResolver) lookup(id string, f *ownershipFlight, started time.Time) {
	ctx, cancel := context.WithTimeout(c.ctx, ownershipLookupTimeout)
	defer cancel()
	f.route, f.err = c.source.ResolveSandbox(ctx, id)
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := ownershipCacheTTL
	if f.err != nil {
		// Briefly coalesce failures as well as successes during an outage.
		ttl = 100 * time.Millisecond
	}
	expires := started.Add(ttl)
	if c.ctx.Err() == nil && c.now().Before(expires) {
		if c.lru.Len() == ownershipCacheSize {
			oldest := c.lru.Back()
			delete(c.entries, oldest.Value.(ownershipEntry).id)
			c.lru.Remove(oldest)
		}
		c.entries[id] = c.lru.PushFront(ownershipEntry{id: id, route: f.route, err: f.err, expires: expires})
	}
	delete(c.flights, id)
	if c.freed != nil {
		close(c.freed)
		c.freed = nil
	}
	close(f.done)
}
