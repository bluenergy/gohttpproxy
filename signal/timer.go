package signal

import (
	"context"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"sync"
	"time"
)

type ActivityUpdater interface {
	Update()
}

type ActivityTimer struct {
	sync.RWMutex
	updated     chan struct{}
	onTimeout   func()
	timerClosed bool
	updateLock  sync.Mutex
}

func (t *ActivityTimer) Update() {
	if t.updateLock.TryLock() {
		defer t.updateLock.Unlock()
		t.updated <- struct{}{}
	}
}

func (t *ActivityTimer) check() {
	select {
	case <-t.updated:
	default:
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
		updated:   make(chan struct{}, 1),
		onTimeout: cancel,
	}
	timer.SetTimeout(timeout)
	return timer
}
