package idleconn

import (
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"net"
)

type IdleTimeoutConnV3 struct {
	update  func()
	Conn    net.Conn
	Updated chan bool
}

func NewIdleTimeoutConnV3(conn net.Conn, fn func()) *IdleTimeoutConnV3 {
	c := &IdleTimeoutConnV3{
		Conn:    conn,
		update:  fn,
		Updated: make(chan bool),
	}
	log.Infof(" create NewIdleTimeoutConnV3")
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
		go ic.update()
	default:
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
