// Generation registration, the refcount flip of doc 02 section 5: a
// query pins exactly one generation per shard for its whole life, a
// publish swaps the pointer under a short lock, and the old generation
// unmaps only when its last query drains. No lock is held across query
// execution and no query ever observes a partial swap.

package serve

import (
	"fmt"
	"sync"
)

// Registry maps shard numbers to their current mounted generation.
type Registry struct {
	mu     sync.Mutex
	shards map[uint16]*genRef
}

type genRef struct {
	m       *Mount
	refs    int
	retired bool
}

func NewRegistry() *Registry {
	return &Registry{shards: make(map[uint16]*genRef)}
}

// Publish swaps m in as its shard's current generation. The previous
// generation, if any, is retired: it closes immediately when idle,
// otherwise when its last Acquire releases.
func (r *Registry) Publish(m *Mount) error {
	r.mu.Lock()
	old := r.shards[m.Shard.Header.Shard]
	r.shards[m.Shard.Header.Shard] = &genRef{m: m}
	var drained *Mount
	if old != nil {
		old.retired = true
		if old.refs == 0 {
			drained = old.m
		}
	}
	r.mu.Unlock()
	if drained != nil {
		return drained.Close()
	}
	return nil
}

// Acquire pins the shard's current generation and returns it with a
// release func. Release must run exactly once, when the query is done
// reading; the last release of a retired generation closes it.
func (r *Registry) Acquire(shard uint16) (*Mount, func(), error) {
	r.mu.Lock()
	g := r.shards[shard]
	if g == nil {
		r.mu.Unlock()
		return nil, nil, fmt.Errorf("serve: shard %d not mounted", shard)
	}
	g.refs++
	r.mu.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			r.mu.Lock()
			g.refs--
			drained := g.retired && g.refs == 0
			r.mu.Unlock()
			if drained {
				_ = g.m.Close()
			}
		})
	}
	return g.m, release, nil
}
