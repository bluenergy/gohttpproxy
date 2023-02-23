package proxyutil

import (
	"context"
	"errors"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"sync"
	"time"
)

type CloseAble interface {
	Close() error
}

type BoxIConn[T CloseAble] struct {
	Label string
	Ok    bool
	Item  T
}

type PoolConn[T CloseAble] struct {
	cpConn chan T

	DialFuncRegistered sync.Once
	DialFuncForPool    func() (T, error)

	MaxPoolConns  int32
	LogLimitCount int
	SleepInterval time.Duration
	BatchSize     int
	PdChan        chan int
}

func NewPoolConnWithOptions[T CloseAble](maxPoolConns int32, logLimitCount int, bSize int, sl time.Duration) *PoolConn[T] {

	p := &PoolConn[T]{
		DialFuncRegistered: sync.Once{},
		cpConn:             make(chan T, maxPoolConns),
		MaxPoolConns:       maxPoolConns,
		LogLimitCount:      logLimitCount,
		BatchSize:          bSize,
		SleepInterval:      sl,
		PdChan:             make(chan int, maxPoolConns),
	}
	// 获取失败，再补一个
	return p
}

func (sp *PoolConn[T]) BackgroundJob() {
	for {
		select {
		case <-sp.PdChan:
			go sp.AsyncFillPool(true)
		case <-time.After(100 * time.Millisecond):

		}
	}
}

func (sp *PoolConn[T]) AsyncFillPool(ignoreLimit bool) {
	cm := "AsyncFillPool@poolutils.go"

	var nilType = new(T)
	tmpc, err := sp.DialFuncForPool()

	if err == nil && &tmpc != nilType {

		select {
		case sp.cpConn <- tmpc:
			go log.Infof(cm + "  补充了一个连接数")
		case <-time.After(1 * time.Second):

			_ = tmpc.Close()

			go log.Infof(cm + "  1秒过去了，仍然没能补充链接，先关掉")
		}

	} else {
		go log.Infof(cm+" 拨号失败：%v", err.Error())
	}

}

func (sp *PoolConn[T]) RegisterDialer(dialerFunc func() (T, error)) {
	go sp.DialFuncRegistered.Do(func() {
		sp.DialFuncForPool = dialerFunc
		go sp.BackgroundJob()
	})
}

func (sp *PoolConn[T]) PickConnOrDialDirect() (t T, err error) {
	go func() {

		select {
		case sp.PdChan <- 1:
		case <-time.After(100 * time.Millisecond):

		}
	}()

	var nilType T
	select {

	case cnn := <-sp.cpConn:
		return cnn, nil
	case <-time.After(1 * time.Second):
		return nilType, errors.New(" failed to obtain conn, will try later")
	}
}
func (sp *PoolConn[T]) PickConnOrDial(ctx context.Context, dialerFunc func() (T, error)) (t T, err error) {
	go sp.DialFuncRegistered.Do(func() {
		sp.DialFuncForPool = dialerFunc
		go sp.BackgroundJob()
	})

	return sp.PickConnOrDialDirect()

}
