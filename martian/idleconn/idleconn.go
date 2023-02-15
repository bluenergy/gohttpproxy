package idleconn

import (
	"net"
	"time"
)

type IdleTimeoutConnV3 struct {
	update func()
	Conn   net.Conn
}

func NewIdleTimeoutConnV3(conn net.Conn, fn func()) *IdleTimeoutConnV3 {
	c := &IdleTimeoutConnV3{
		Conn:   conn,
		update: fn,
	}
	return c
}

func (ic *IdleTimeoutConnV3) Read(buf []byte) (int, error) {
	go ic.UpdateIdleTime()
	return ic.Conn.Read(buf)
}

func (ic *IdleTimeoutConnV3) UpdateIdleTime() {
	_ = ic.Conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	_ = ic.Conn.SetWriteDeadline(time.Now().Add(45 * time.Second))
	ic.update()
}

func (ic *IdleTimeoutConnV3) Write(buf []byte) (int, error) {
	go ic.UpdateIdleTime()
	return ic.Conn.Write(buf)
}

func (c *IdleTimeoutConnV3) Close() {
	if c.Conn != nil {
		_ = c.Conn.Close()
	}
}
