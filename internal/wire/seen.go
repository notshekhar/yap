package wire

import (
	"container/list"
	"sync"
	"time"
)

// Seen is the memory that stops a flood from becoming a storm.
//
// Every node rebroadcasts what it receives, so in a room of n mutually visible
// peers a single message would otherwise loop until its TTL burned out,
// costing O(n^2) frames. Recording message IDs and dropping repeats collapses
// that back to one relay per node per message. It is the single most important
// piece of the mesh, and it is deliberately bounded in both size and time:
// unbounded, it is a memory leak with a nice name.
type Seen struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	items map[MsgID]*list.Element
	order *list.List // front = newest
	now   func() time.Time
}

type seenEntry struct {
	id       MsgID
	deadline time.Time
}

// NewSeen returns a dedupe set holding at most max ids for at most ttl.
func NewSeen(max int, ttl time.Duration) *Seen {
	if max <= 0 {
		max = 4096
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &Seen{
		max:   max,
		ttl:   ttl,
		items: make(map[MsgID]*list.Element, max),
		order: list.New(),
		now:   time.Now,
	}
}

// Mark records an id and reports whether it is new. A false return means the
// packet has been handled already and must not be relayed again.
func (s *Seen) Mark(id MsgID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if el, ok := s.items[id]; ok {
		entry := el.Value.(*seenEntry)
		if now.Before(entry.deadline) {
			// Refreshing the deadline here would let a peer keep an id alive
			// forever by re-sending it. Leave it to expire on its own clock.
			s.order.MoveToFront(el)
			return false
		}
		// Expired: drop the stale record and treat the id as new again.
		s.order.Remove(el)
		delete(s.items, id)
	}

	s.evictExpiredLocked(now)

	el := s.order.PushFront(&seenEntry{id: id, deadline: now.Add(s.ttl)})
	s.items[id] = el

	for s.order.Len() > s.max {
		oldest := s.order.Back()
		s.order.Remove(oldest)
		delete(s.items, oldest.Value.(*seenEntry).id)
	}
	return true
}

// Len reports how many ids are currently remembered.
func (s *Seen) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked(s.now())
	return s.order.Len()
}

// evictExpiredLocked walks from the oldest end, which is where expiry
// concentrates, and stops at the first live entry.
func (s *Seen) evictExpiredLocked(now time.Time) {
	for {
		oldest := s.order.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*seenEntry)
		if now.Before(entry.deadline) {
			return
		}
		s.order.Remove(oldest)
		delete(s.items, entry.id)
	}
}
