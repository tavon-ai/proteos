package machine

import (
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/store"
)

// Update is one published machine change: the post-commit machine row plus the
// audit event that recorded the change. The event id is the SSE Last-Event-ID.
//
// Deleted marks the terminal "machine destroyed" notification: the row no longer
// exists, so Machine carries only the pre-delete row (for the SSE user-id filter)
// and Event is zero. Subscribers emit a `destroyed` event instead of a `machine`
// one.
//
// Shutdown marks the sentinel notification sent to all SSE subscribers just
// before the server begins draining connections. Subscribers should emit a
// "shutdown" event and close.
type Update struct {
	Machine  store.Machine
	Event    store.MachineEvent
	Deleted  bool
	Shutdown bool
}

// Broker is an in-process pub/sub fan-out for machine Updates. It is the
// embryo of Phase 11's LISTEN/NOTIFY: proportionate for one control-plane
// instance and one dashboard per user. Updates are published *after* the DB
// commit, so a subscriber never observes an event that isn't durable.
//
// Per-user SSE subscribers (one per open dashboard, so this set scales with
// the number of active users) register interest by user id via Subscribe, so
// Publish only visits the subscribers for the update's owning user
// (O(subscribers for that user)) instead of every connected client (O(all
// subscribers)) — the latter doesn't scale once many users are watching
// dashboards concurrently. A small, fixed number of in-process components
// (metrics, guestctl) need every user's updates regardless of ownership; they
// register via SubscribeAll instead, and Publish always visits that set too —
// its size does not grow with user/connection count.
type Broker struct {
	mu     sync.Mutex
	nextID int
	subs   map[pgtype.UUID]map[int]chan Update // per-user SSE subscribers
	all    map[int]chan Update                 // subscribers interested in every user's updates
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[pgtype.UUID]map[int]chan Update), all: make(map[int]chan Update)}
}

// Subscribe registers a subscriber interested in userID's updates and returns
// its channel plus a cancel func that unregisters and closes it. The channel
// is buffered; if a slow consumer fills it, further updates for that
// subscriber are dropped — the SSE client recovers the gap on reconnect via
// Last-Event-ID replay from the DB.
func (b *Broker) Subscribe(userID pgtype.UUID) (<-chan Update, func()) {
	ch := make(chan Update, 32)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	if b.subs[userID] == nil {
		b.subs[userID] = make(map[int]chan Update)
	}
	b.subs[userID][id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if peers, ok := b.subs[userID]; ok {
			if c, ok := peers[id]; ok {
				delete(peers, id)
				close(c)
			}
			if len(peers) == 0 {
				delete(b.subs, userID)
			}
		}
		b.mu.Unlock()
	}
}

// SubscribeAll registers a subscriber interested in every user's updates
// (e.g. metrics, guestctl) and returns its channel plus a cancel func that
// unregisters and closes it. Buffering/drop semantics match Subscribe.
func (b *Broker) SubscribeAll() (<-chan Update, func()) {
	ch := make(chan Update, 32)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.all[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if c, ok := b.all[id]; ok {
			delete(b.all, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// Shutdown notifies all current subscribers (across every user, plus the
// all-user subscribers) that the server is shutting down. Each subscriber
// receives one Update with Shutdown set; they are expected to emit a final
// "shutdown" SSE event and return. This is the one case that legitimately
// visits every subscriber.
func (b *Broker) Shutdown() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, peers := range b.subs {
		for _, ch := range peers {
			select {
			case ch <- Update{Shutdown: true}:
			default:
			}
		}
	}
	for _, ch := range b.all {
		select {
		case ch <- Update{Shutdown: true}:
		default:
		}
	}
}

// Publish fans out u to the current subscribers for u's owning user plus the
// all-user subscribers, dropping for any whose buffer is full rather than
// blocking the caller (which holds no DB lock here, but we still never want a
// stuck SSE client to stall a transition).
func (b *Broker) Publish(u Update) {
	if b == nil {
		return // publishing is optional (e.g. in unit tests without SSE)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[u.Machine.UserID] {
		select {
		case ch <- u:
		default:
		}
	}
	for _, ch := range b.all {
		select {
		case ch <- u:
		default:
		}
	}
}
