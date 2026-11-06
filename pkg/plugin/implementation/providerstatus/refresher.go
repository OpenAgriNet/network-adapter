package providerstatus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

// snapshot is one successful read of the registry's active providers.
type snapshot struct {
	ids []string
	set map[string]struct{}
	at  time.Time
}

// refresher keeps the active provider list in memory and re-reads it in the
// background, so no request waits on the registry.
type refresher struct {
	lister          activeProviderLister
	participantType string
	role            string
	interval        time.Duration
	maxStaleness    time.Duration
	now             func() time.Time

	cur       atomic.Pointer[snapshot]
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newRefresher(l activeProviderLister, participantType, role string, interval, maxStaleness time.Duration) *refresher {
	return &refresher{
		lister:          l,
		participantType: participantType,
		role:            role,
		interval:        interval,
		maxStaleness:    maxStaleness,
		now:             time.Now,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// refresh reads the list once, within one interval. On failure the previous
// list stays in use.
func (r *refresher) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.interval)
	defer cancel()
	ids, err := r.lister.ActiveProviderIDs(ctx, r.participantType, r.role)
	if err != nil {
		return err
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	r.cur.Store(&snapshot{ids: ids, set: set, at: r.now()})
	return nil
}

// start refreshes every interval until close. One loop, so refreshes never
// overlap: a slow one delays the next tick instead of piling up.
func (r *refresher) start() {
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				if err := r.refresh(context.Background()); err != nil {
					log.Errorf(context.Background(), err, "providerstatus: could not refresh the active provider list, %s", r.age())
				}
			}
		}
	}()
}

// active returns the current list, or an error when there is none yet or it is
// older than maxStaleness: an unanswered question must never read as "active".
func (r *refresher) active() (*snapshot, error) {
	snap := r.cur.Load()
	if snap == nil {
		return nil, fmt.Errorf("could not check provider status: the active provider list has not been loaded yet")
	}
	if age := r.now().Sub(snap.at); age > r.maxStaleness {
		return nil, fmt.Errorf("could not check provider status: the active provider list is %s old, older than maxStaleness %s",
			age.Round(time.Second), r.maxStaleness)
	}
	return snap, nil
}

// age describes the list in use, for logs.
func (r *refresher) age() string {
	snap := r.cur.Load()
	if snap == nil {
		return "no list loaded yet"
	}
	return fmt.Sprintf("serving the list from %s ago", r.now().Sub(snap.at).Round(time.Second))
}

// close stops the background loop. Safe to call more than once, and before
// start only stops a loop that never ran.
func (r *refresher) close() error {
	r.closeOnce.Do(func() { close(r.stop) })
	return nil
}
