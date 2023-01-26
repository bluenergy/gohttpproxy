package signal

import (
	"context"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"sync"
	"sync/atomic"
	"time"
)

type ActivityUpdater interface {
	Update()
}

type ActivityTimer struct {
	sync.RWMutex
	updated     atomic.Int64
	tTimeout    time.Duration
	onTimeout   func()
	timerClosed bool
	updateLock  sync.Mutex
}

func (t *ActivityTimer) Update() {
	if t.updateLock.TryLock() {
		defer t.updateLock.Unlock()
		tsn := time.Now().Add(t.tTimeout).UnixMilli()
		log.Infof("update timer for ActivityTimer:%v", tsn)
		go t.updated.Swap(tsn)
	}
}

func (t *ActivityTimer) check() {
	ttn := t.updated.Load()
	if ttn <= 0 || ttn < time.Now().UnixMilli() {
		t.finish()
	}
}

func (t *ActivityTimer) finish() {
	t.Lock()
	defer t.Unlock()

	t.timerClosed = true
	if t.onTimeout != nil {
		t.onTimeout()
		t.onTimeout = nil
	}
}

func (t *ActivityTimer) SetTimeout(timeout time.Duration) {
	t.tTimeout = timeout
	if timeout == 0 {
		t.finish()
		return
	}

	//过N 秒，执行一次 check
	t.Update()
	go func() {
		for {
			if t.timerClosed {
				log.Infof("ActivityTimer finish and close")
				break
			}
			time.Sleep(timeout)
			t.check()
		}
	}()
}

func CancelAfterInactivity(ctx context.Context, cancel func(), timeout time.Duration) *ActivityTimer {
	timer := &ActivityTimer{
		updated:   atomic.Int64{},
		onTimeout: cancel,
	}
	timer.SetTimeout(timeout)
	return timer
}
