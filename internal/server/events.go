package server

import "sync"

// Event is something clients see live, such as download progress.
type Event struct {
	Type string
	Data any
}

// broker fans events out to open streams and closes streams whose token is
// revoked.
type broker struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

type subscriber struct {
	token int64
	ch    chan Event
	done  chan struct{}
	once  sync.Once
}

func newBroker() *broker { return &broker{subs: map[*subscriber]struct{}{}} }

func (b *broker) subscribe(token int64) *subscriber {
	s := &subscriber{token: token, ch: make(chan Event, 64), done: make(chan struct{})}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *broker) remove(s *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked(s)
}

func (b *broker) closeLocked(s *subscriber) {
	delete(b.subs, s)
	s.once.Do(func() { close(s.done) })
}

// publish sends e to every stream. A stream that cannot keep up is closed;
// its client reconnects and reloads state.
func (b *broker) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			b.closeLocked(s)
		}
	}
}

// closeToken closes all streams opened with the token and returns how many.
func (b *broker) closeToken(id int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for s := range b.subs {
		if s.token == id {
			b.closeLocked(s)
			n++
		}
	}
	return n
}

func (b *broker) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		b.closeLocked(s)
	}
}

func (b *broker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
