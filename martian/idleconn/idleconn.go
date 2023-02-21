package idleconn

import (
	"net"
	"sync/atomic"
	"time"
)

type IdleTimeoutConnV3 struct {
	update     func()
	Conn       net.Conn
	Updated    chan bool
	AfterDelay atomic.Int64
}

func NewIdleTimeoutConnV3(conn net.Conn, fn func()) *IdleTimeoutConnV3 {
	ch := make(chan bool)
	go func() {
		ch <- true
	}()
	c := &IdleTimeoutConnV3{
		Conn:    conn,
		update:  fn,
		Updated: ch,
	}
	//必须先注入一个 到chan 不然执行总会失败

	return c
}

func (ic *IdleTimeoutConnV3) Read(buf []byte) (int, error) {

	go ic.UpdateIdleTime()
	select {
	case ic.Updated <- true:
	default:
	}
	return ic.Conn.Read(buf)
}

func (ic *IdleTimeoutConnV3) UpdateIdleTime() {

	select {
	case <-ic.Updated:
		la := ic.AfterDelay.Load()
		ic.AfterDelay.Store(time.Now().Add(5 * time.Second).Unix())
		if la <= 0 || la <= time.Now().Unix() {
			//log.Infof(" UpdateIdleTime for now")
			go ic.update()
		}
	}
}

func (ic *IdleTimeoutConnV3) Write(buf []byte) (int, error) {

	go ic.UpdateIdleTime()
	select {
	case ic.Updated <- true:
	default:
	}
	return ic.Conn.Write(buf)
}

func (c *IdleTimeoutConnV3) Close() {
	if c.Conn != nil {
		_ = c.Conn.Close()
	}
}
