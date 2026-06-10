package machine

import (
	"sync"

	"github.com/tavon/proteos/controlplane/internal/store"
)

// Update is one published machine change: the post-commit machine row plus the
// audit event that recorded the change. The event id is the SSE Last-Event-ID.
type Update struct {
	Machine store.Machine
	Event   store.MachineEvent
}

// Broker is an in-process pub/sub fan-out for machine Updates. It is the
// embryo of Phase 11's LISTEN/NOTIFY: proportionate for one control-plane
// instance and one dashboard per user. Updates are published *after* the DB
// commit, so a subscriber never observes an event that isn't durable.
type Broker struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan Update
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[int]chan Update)}
}

// Subscribe registers a subscriber and returns its channel plus a cancel func
// that unregisters and closes it. The channel is buffered; if a slow consumer
// fills it, further updates for that subscriber are dropped — the SSE client
// recovers the gap on reconnect via Last-Event-ID replay from the DB.
func (b *Broker) Subscribe() (<-chan Update, func()) {
	ch := make(chan Update, 32)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// Publish fans out u to every current subscriber, dropping for any whose buffer
// is full rather than blocking the caller (which holds no DB lock here, but we
// still never want a stuck SSE client to stall a transition).
func (b *Broker) Publish(u Update) {
	if b == nil {
		return // publishing is optional (e.g. in unit tests without SSE)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- u:
		default:
		}
	}
}
