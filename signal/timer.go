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
	updated      atomic.Int64
	timeInterval time.Duration
	onTimeout    func()
	timerClosed  bool
	updateLock   sync.Mutex
}

func (t *ActivityTimer) Update() {
	if t.updateLock.TryLock() {
		defer t.updateLock.Unlock()
		t.updated.Store(time.Now().Add(t.timeInterval).UnixMilli())

	}
}

func (t *ActivityTimer) check() {
	tn := time.Now().UnixMilli()
	if tn > t.updated.Load() {
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
		onTimeout:    cancel,
		timeInterval: timeout,
	}
	timer.SetTimeout(timeout)
	return timer
}
