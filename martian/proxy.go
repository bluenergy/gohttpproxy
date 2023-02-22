// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package martian

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"github.com/dgraph-io/ristretto"
	"github.com/gohttpproxy/gohttpproxy/martian/idleconn"
	"github.com/gohttpproxy/gohttpproxy/martian/task"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v3"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"github.com/gohttpproxy/gohttpproxy/martian/mitm"
	"github.com/gohttpproxy/gohttpproxy/martian/nosigpipe"
	"github.com/gohttpproxy/gohttpproxy/martian/proxyutil"
	"github.com/gohttpproxy/gohttpproxy/martian/retry"
	"github.com/gohttpproxy/gohttpproxy/martian/trafficshape"
	"github.com/gohttpproxy/gohttpproxy/signal"
)

const MaxRetries = 7
const RetryAfterTime = 15
const MaxRetryIntervalTime = 25

var errClose = errors.New("closing connection")
var noop = Noop("martian")
var DefaultProxyIdleTimeout = 45 * time.Second

//增加idle conn

func isCloseable(err error) bool {
	if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		return true
	}

	switch err {
	case io.EOF, io.ErrClosedPipe, errClose:
		return true
	}

	return false
}

// Proxy is an HTTP proxy with support for TLS MITM and customizable behavior.
type Proxy struct {
	roundTripper http.RoundTripper
	//带下级代理的RoundTripper
	dsRoundTripper http.RoundTripper
	dial           func(string, string) (net.Conn, error)
	timeout        time.Duration
	mitm           *mitm.Config
	proxyURL       *url.URL
	conns          sync.WaitGroup
	connsMu        sync.Mutex // protects conns.Add/Wait from concurrent access
	closing        chan bool

	reqmod RequestModifier
	resmod ResponseModifier
	Cache  *ristretto.Cache
}

// NewProxy returns a new HTTP proxy.
func NewProxy() *Proxy {
	proxy := &Proxy{
		roundTripper: &http.Transport{
			// TODO(adamtanner): This forces the http.Transport to not upgrade requests
			// to HTTP/2 in Go 1.6+. Remove this once Martian can support HTTP/2.
			TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
			//Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   3 * time.Second,
			DisableKeepAlives:     true,
			MaxIdleConns:          0,
			MaxIdleConnsPerHost:   0,
			ExpectContinueTimeout: 2 * time.Second,
		},
		dsRoundTripper: &http.Transport{
			TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
			//Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   3 * time.Second,
			DisableKeepAlives:     true,
			MaxIdleConns:          0,
			MaxIdleConnsPerHost:   0,
			ExpectContinueTimeout: 2 * time.Second,
		},
		timeout: 0,
		closing: make(chan bool),
		reqmod:  noop,
		resmod:  noop,
	}
	proxy.SetDial((&net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}).Dial)
	return proxy
}

// GetRoundTripper gets the http.RoundTripper of the proxy.
func (p *Proxy) GetRoundTripper() http.RoundTripper {
	return p.roundTripper
}

// SetRoundTripper sets the http.RoundTripper of the proxy.
func (p *Proxy) SetRoundTripper(rt http.RoundTripper) {
	p.roundTripper = rt

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		//	tr.Proxy = http.ProxyURL(p.proxyURL)
		tr.Dial = p.dial
	}
}

// SetDownstreamProxy sets the proxy that receives requests from the upstream
// proxy.
func (p *Proxy) SetDownstreamProxy(proxyURL *url.URL) {
	p.proxyURL = proxyURL

	if tr, ok := p.dsRoundTripper.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(p.proxyURL)
	}
}

// SetTimeout sets the request timeout of the proxy.
func (p *Proxy) SetTimeout(timeout time.Duration) {
	p.timeout = timeout
}

// SetMITM sets the config to use for MITMing of CONNECT requests.
func (p *Proxy) SetMITM(config *mitm.Config) {
	p.mitm = config
}

// SetDial sets the dial func used to establish a connection.
func (p *Proxy) SetDial(dial func(string, string) (net.Conn, error)) {
	p.dial = func(a, b string) (net.Conn, error) {
		c, e := dial(a, b)
		nosigpipe.IgnoreSIGPIPE(c)
		return c, e
	}

	if tr, ok := p.roundTripper.(*http.Transport); ok {
		tr.Dial = p.dial
	}
}

// Close sets the proxy to the closing state so it stops receiving new connections,
// finishes processing any inflight requests, and closes existing connections without
// reading anymore requests from them.
func (p *Proxy) Close() {
	log.Infof("martian: closing down proxy")

	close(p.closing)

	log.Infof("martian: waiting for connections to close")
	p.connsMu.Lock()
	p.conns.Wait()
	p.connsMu.Unlock()
	log.Infof("martian: all connections closed")
}

// Closing returns whether the proxy is in the closing state.
func (p *Proxy) Closing() bool {
	select {
	case <-p.closing:
		return true
	default:
		return false
	}
}

// SetRequestModifier sets the request modifier.
func (p *Proxy) SetRequestModifier(reqmod RequestModifier) {
	if reqmod == nil {
		reqmod = noop
	}

	p.reqmod = reqmod
}

// SetResponseModifier sets the response modifier.
func (p *Proxy) SetResponseModifier(resmod ResponseModifier) {
	if resmod == nil {
		resmod = noop
	}

	p.resmod = resmod
}

// Serve accepts connections from the listener and handles the requests.
func (p *Proxy) Serve(l net.Listener) error {
	defer l.Close()

	var delay time.Duration
	for {
		if p.Closing() {
			return nil
		}

		conn, err := l.Accept()
		nosigpipe.IgnoreSIGPIPE(conn)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				if delay == 0 {
					delay = 50 * time.Millisecond
				} else {
					delay *= 2
				}
				if max := time.Second; delay > max {
					delay = max
				}

				log.Debugf("martian: temporary error on accept: %v", err)
				time.Sleep(delay)
				continue
			}

			if errors.Is(err, net.ErrClosed) {
				log.Debugf("martian: listener closed, returning")
				return err
			}

			log.Errorf("martian: failed to accept: %v", err)
			return err
		}
		delay = 0
		log.Debugf("martian: accepted connection from %s", conn.RemoteAddr())

		/*if tconn, ok := conn.(*net.TCPConn); ok {
			tconn.SetKeepAlive(true)
			tconn.SetKeepAlivePeriod(3 * time.Minute)
		}*/

		go func() {
			p.HandleLoop(conn)
		}()
	}
}

func (p *Proxy) HandleLoop(conn net.Conn) {
	p.connsMu.Lock()
	p.conns.Add(1)
	p.connsMu.Unlock()
	defer p.conns.Done()
	if p.Closing() {
		return
	}

	brw := bufio.NewReadWriter(bufio.NewReaderSize(conn, DefaultReadBufSize), bufio.NewWriterSize(conn, DefaultWriteBufSize))

	s, err := newSession(conn, brw)
	if err != nil {
		log.Errorf("martian: failed to create session: %v", err)
		return
	}

	ctx, err := withSession(s)
	if err != nil {
		log.Errorf("martian: failed to create context: %v", err)
		return
	}

	for {
		/*deadline := time.Now().Add(p.timeout)
		conn.SetDeadline(deadline)*/

		if err := p.handle(ctx, conn, brw); isCloseable(err) {
			log.Debugf("martian: closing connection: %v", conn.RemoteAddr())
			return
		}
	}
}

func (p *Proxy) readRequest(ctx *Context, conn net.Conn, brw *bufio.ReadWriter) (*http.Request, error) {
	var req *http.Request
	reqc := make(chan *http.Request, 1)
	errc := make(chan error, 1)
	go func() {
		r, err := http.ReadRequest(brw.Reader)
		if err != nil {
			errc <- err
			return
		}
		reqc <- r
	}()
	select {
	case err := <-errc:
		if isCloseable(err) {
			log.Debugf("martian: connection closed prematurely: %v", err)
		} else {
			log.Errorf("martian: failed to read request: %v", err)
		}

		// TODO: TCPConn.WriteClose() to avoid sending an RST to the client.

		return nil, errClose
	case req = <-reqc:
	case <-p.closing:
		return nil, errClose
	}

	return req, nil
}

func (p *Proxy) handleConnectRequest(ctx *Context, req *http.Request, session *Session, brw *bufio.ReadWriter, conn net.Conn) error {
	cm := "handleConnectRequest@proxy.go"
	//cm := "handleConnectRequest@proxy.go"
	if err := p.reqmod.ModifyRequest(req); err != nil {
		log.Errorf(cm+" martian: error modifying CONNECT request: %v", err)
		proxyutil.Warning(req.Header, err)
	}
	if session.Hijacked() {
		log.Debugf(cm + " martian: connection hijacked by request modifier")
		return nil
	}

	if p.mitm != nil {
		log.Debugf(cm+" martian: attempting MITM for connection: %s / %s", req.Host, req.URL.String())

		res := proxyutil.NewResponse(200, nil, req)

		if err := p.resmod.ModifyResponse(res); err != nil {
			log.Errorf(cm+" martian: error modifying CONNECT response: %v", err)
			proxyutil.Warning(res.Header, err)
		}
		if session.Hijacked() {
			log.Infof(cm + " martian: connection hijacked by response modifier")
			return nil
		}

		if err := res.Write(brw); err != nil {
			log.Errorf(cm+" martian: got error while writing response back to client: %v", err)
		}
		if err := brw.Flush(); err != nil {
			log.Errorf(cm+" martian: got error while flushing response back to client: %v", err)
		}

		log.Debugf(cm+" martian: completed MITM for connection: %s", req.Host)

		b := make([]byte, 1)
		if _, err := brw.Read(b); err != nil {
			log.Errorf(cm+" martian: error peeking message through CONNECT tunnel to determine type: %v", err)
		}

		// Drain all of the rest of the buffered data.
		buf := make([]byte, brw.Reader.Buffered())
		brw.Read(buf)

		// 22 is the TLS handshake.
		// https://tools.ietf.org/html/rfc5246#section-6.2.1
		if b[0] == 22 {
			// Prepend the previously read data to be read again by
			// http.ReadRequest.
			tlsconn := tls.Server(&peekedConn{conn, io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn)}, p.mitm.TLSForHost(req.Host))

			if err := tlsconn.Handshake(); err != nil {
				p.mitm.HandshakeErrorCallback(req, err)
				return err
			}
			if tlsconn.ConnectionState().NegotiatedProtocol == "h2" {
				return p.mitm.H2Config().Proxy(p.closing, tlsconn, req.URL)
			}

			var nconn net.Conn
			nconn = tlsconn
			// If the original connection is a traffic shaped connection, wrap the tls
			// connection inside a traffic shaped connection too.
			if ptsconn, ok := conn.(*trafficshape.Conn); ok {
				nconn = ptsconn.Listener.GetTrafficShapedConn(tlsconn)
			}
			brw.Writer.Reset(nconn)
			brw.Reader.Reset(nconn)
			return p.handle(ctx, nconn, brw)
		}

		// Prepend the previously read data to be read again by http.ReadRequest.
		brw.Reader.Reset(io.MultiReader(bytes.NewReader(b), bytes.NewReader(buf), conn))
		return p.handle(ctx, conn, brw)
	}

	log.Debugf(cm+" martian: attempting to establish CONNECT tunnel: %s", req.URL.Host)
	res, cconn, cerr := p.connect(req)
	if cerr != nil {
		log.Errorf(cm+" martian: failed to CONNECT: %v", cerr)
		res = proxyutil.NewResponse(502, nil, req)
		proxyutil.Warning(res.Header, cerr)

		if err := p.resmod.ModifyResponse(res); err != nil {
			log.Errorf(cm+" martian: error modifying CONNECT response: %v", err)
			proxyutil.Warning(res.Header, err)
		}
		if session.Hijacked() {
			log.Infof(cm + " martian: connection hijacked by response modifier")
			return nil
		}

		if err := res.Write(brw); err != nil {
			log.Errorf(cm+"martian: got error while writing response back to client: %v", err)
		}
		err := brw.Flush()
		if err != nil {
			log.Errorf("martian: got error while flushing response back to client: %v", err)
		}
		return err
	}
	defer res.Body.Close()

	if err := p.resmod.ModifyResponse(res); err != nil {
		log.Errorf("martian: error modifying CONNECT response: %v", err)
		proxyutil.Warning(res.Header, err)
	}
	if session.Hijacked() {
		log.Infof("martian: connection hijacked by response modifier")
		return nil
	}

	res.ContentLength = -1
	if err := res.Write(brw); err != nil {
		log.Errorf("martian: got error while writing response back to client: %v", err)
	}
	if err := brw.Flush(); err != nil {
		log.Errorf("martian: got error while flushing response back to client: %v", err)
	}

	connCtx, cancel := context.WithCancel(context.Background())
	cancelFunc := func() {
		log.Infof(cm + " 链接已经超时，准备关闭链接")
		cancel()

		dlTime := time.Now().Add(DefaultProxyIdleTimeout)
		if cconn != nil {
			cconn.SetDeadline(dlTime)
		}
		if conn != nil {
			conn.SetDeadline(dlTime)
		}
	}
	timer := signal.CancelAfterInactivity(connCtx, cancelFunc, DefaultProxyIdleTimeout)

	updateFunc := func() {
		timer.Update()
	}

	idleCbw := idleconn.NewIdleTimeoutConnV3(cconn, updateFunc)

	idleCbr := idleconn.NewIdleTimeoutConnV3(cconn, updateFunc)

	idleBrw := idleconn.NewIdleTimeoutConnV3(conn, updateFunc)

	copySync := func(w io.Writer, r io.Reader) error {

		if _, err := io.Copy(w, r); err != nil && err != io.EOF {
			log.Errorf(cm+" martian: failed to copy CONNECT tunnel: %v", err)
			return err
		}

		log.Debugf(cm + " martian: CONNECT tunnel finished copying")
		return nil
	}

	_ = task.Run(
		context.Background(),
		func() error {
			return copySync(idleCbr, idleBrw)
		},
		func() error {
			return copySync(idleBrw, idleCbw)
		},
	)

	log.Debugf(cm + " martian: established CONNECT tunnel, proxying traffic")

	log.Debugf(cm + " io.Copy 关闭，准备关闭链接")

	log.Debugf(cm + " martian: closed CONNECT tunnel")

	dlTime := time.Now().Add(DefaultProxyIdleTimeout)
	if cconn != nil {
		cconn.SetDeadline(dlTime)
	}
	if conn != nil {
		conn.SetDeadline(dlTime)
	}

	log.Infof(cm + " 所有链接已关闭")

	return errClose
}

func (p *Proxy) handle(ctx *Context, conn net.Conn, brw *bufio.ReadWriter) error {
	cm := "handle@proxy.go"
	log.Debugf("martian: waiting for request: %v", conn.RemoteAddr())

	req, err := p.readRequest(ctx, conn, brw)
	if err != nil {
		return err
	}
	defer req.Body.Close()

	session := ctx.Session()
	ctx, err = withSession(session)
	if err != nil {
		log.Errorf("martian: failed to build new context: %v", err)
		return err
	}

	link(req, ctx)
	defer unlink(req)

	if tsconn, ok := conn.(*trafficshape.Conn); ok {
		wrconn := tsconn.GetWrappedConn()
		if sconn, ok := wrconn.(*tls.Conn); ok {
			session.MarkSecure()

			cs := sconn.ConnectionState()
			req.TLS = &cs
		}
	}

	if tconn, ok := conn.(*tls.Conn); ok {
		session.MarkSecure()

		cs := tconn.ConnectionState()
		req.TLS = &cs
	}

	log.Infof(cm+" URL: %v", req.URL.String())

	/*	req.URL.Scheme = "http"
		if session.IsSecure() {
			log.Infof("martian: forcing HTTPS inside secure session")
			req.URL.Scheme = "https"
		}*/

	req.RemoteAddr = conn.RemoteAddr().String()
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}

	if req.Method == "CONNECT" {
		return p.handleConnectRequest(ctx, req, session, brw, conn)
	}

	// Not a CONNECT request
	if err := p.reqmod.ModifyRequest(req); err != nil {
		log.Errorf("martian: error modifying request: %v", err)
		proxyutil.Warning(req.Header, err)
	}
	if session.Hijacked() {
		return nil
	}

	// perform the HTTP roundtrip
	res, err := p.roundTrip(ctx, req)
	if err != nil {
		log.Errorf("martian: failed to round trip: %v", err)
		res = proxyutil.NewResponse(502, nil, req)
		proxyutil.Warning(res.Header, err)
	}
	defer res.Body.Close()

	// set request to original request manually, res.Request may be changed in transport.
	// see https://github.com/cnmade/martian/issues/298
	res.Request = req

	if err := p.resmod.ModifyResponse(res); err != nil {
		log.Errorf("martian: error modifying response: %v", err)
		proxyutil.Warning(res.Header, err)
	}
	if session.Hijacked() {
		log.Infof("martian: connection hijacked by response modifier")
		return nil
	}

	var closing error
	if req.Close || res.Close || p.Closing() {
		log.Debugf("martian: received close request: %v", req.RemoteAddr)
		res.Close = true
		closing = errClose
	}

	// check if conn is a traffic shaped connection.
	if ptsconn, ok := conn.(*trafficshape.Conn); ok {
		ptsconn.Context = &trafficshape.Context{}
		// Check if the request URL matches any URLRegex in Shapes. If so, set the connections's Context
		// with the required information, so that the Write() method of the Conn has access to it.
		for urlregex, buckets := range ptsconn.LocalBuckets {
			if match, _ := regexp.MatchString(urlregex, req.URL.String()); match {
				if rangeStart := proxyutil.GetRangeStart(res); rangeStart > -1 {
					dump, err := httputil.DumpResponse(res, false)
					if err != nil {
						return err
					}
					ptsconn.Context = &trafficshape.Context{
						Shaping:            true,
						Buckets:            buckets,
						GlobalBucket:       ptsconn.GlobalBuckets[urlregex],
						URLRegex:           urlregex,
						RangeStart:         rangeStart,
						ByteOffset:         rangeStart,
						HeaderLen:          int64(len(dump)),
						HeaderBytesWritten: 0,
					}
					// Get the next action to perform, if there.
					ptsconn.Context.NextActionInfo = ptsconn.GetNextActionFromByte(rangeStart)
					// Check if response lies in a throttled byte range.
					ptsconn.Context.ThrottleContext = ptsconn.GetCurrentThrottle(rangeStart)
					if ptsconn.Context.ThrottleContext.ThrottleNow {
						ptsconn.Context.Buckets.WriteBucket.SetCapacity(
							ptsconn.Context.ThrottleContext.Bandwidth)
					}
					log.Infof(
						"trafficshape: Request %s with Range Start: %d matches a Shaping request %s. Enforcing Traffic shaping.",
						req.URL, rangeStart, urlregex)
				}
				break
			}
		}
	}

	err = res.Write(brw)
	if err != nil {
		log.Errorf("martian: got error while writing response back to client: %v", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
	}
	err = brw.Flush()
	if err != nil {
		log.Errorf("martian: got error while flushing response back to client: %v", err)
		if _, ok := err.(*trafficshape.ErrForceClose); ok {
			closing = errClose
		}
	}
	return closing
}

// A peekedConn subverts the net.Conn.Read implementation, primarily so that
// sniffed bytes can be transparently prepended.
type peekedConn struct {
	net.Conn
	r io.Reader
}

// Read allows control over the embedded net.Conn's read data. By using an
// io.MultiReader one can read from a conn, and then replace what they read, to
// be read again.
func (c *peekedConn) Read(buf []byte) (int, error) { return c.r.Read(buf) }

func (p *Proxy) roundTrip(ctx *Context, req *http.Request) (*http.Response, error) {
	cm := "roundTrip@proxy.go "
	log.Infof(cm+" req: %+v", req)
	if ctx.SkippingRoundTrip() {
		log.Debugf("martian: skipping round trip")
		return proxyutil.NewResponse(200, nil, req), nil
	}

	//TODO 如果域名不在ch86cidr内，就启用proxy
	shouldUseDsProxy := true

	log.Infof(cm+" URL: %v", req.URL.String())
	addr := req.URL.Host
	log.Infof(cm+" addr: %v", addr)
	sh := strings.Split(addr, ":")
	hostName := sh[0]
	log.Infof(cm+" host: %v", hostName)

	ip, err := net.LookupIP(hostName)
	if err == nil {
		log.Infof(cm+" ip: %v", ip)
		if p.inCh86cidr(ip[0]) == true {
			shouldUseDsProxy = false
			log.Infof(cm + " In ch86cidr , not use ds proxy")
		} else {
			log.Infof(cm + " not in ch86cidr, use ds proxy")
		}
	}

	log.Infof(cm+"p.proxyURL: %+v, shouldUseDsProxy: %+v", p.proxyURL, shouldUseDsProxy)

	if p.proxyURL != nil && shouldUseDsProxy == true {
		if tr, ok := p.dsRoundTripper.(*http.Transport); ok {
			tr.Proxy = http.ProxyURL(p.proxyURL)
			return tr.RoundTrip(req)
		}
	} else {
		if tr, ok := p.roundTripper.(*http.Transport); ok {
			return tr.RoundTrip(req)
		}
	}
	return p.roundTripper.RoundTrip(req)
}

func (p *Proxy) connect(req *http.Request) (*http.Response, net.Conn, error) {
	//TODO check if the host in ch n route
	cm := "connect@proxy.go"

	var err error
	if p.proxyURL != nil {

		shouldUseDsProxy := true

		log.Infof(cm+" URL: %v", req.URL.String())
		addr := req.URL.Host
		log.Infof(cm+" addr: %v", addr)
		sh := strings.Split(addr, ":")
		hostName := sh[0]
		log.Infof(cm+" host: %v", hostName)

		ip, err := net.LookupIP(hostName)
		if err == nil {
			log.Infof(cm+" ip: %v", ip)
			if p.inCh86cidr(ip[0]) == true {
				shouldUseDsProxy = false
				log.Infof(cm + " In ch86cidr , not use ds proxy")
			} else {
				log.Infof(cm + " not in ch86cidr, use ds proxy")
			}
		}

		log.Infof(cm+"p.proxyURL: %+v, shouldUseDsProxy: %+v", p.proxyURL, shouldUseDsProxy)

		if shouldUseDsProxy == true {

			log.Debugf("martian: CONNECT with downstream proxy: %s", p.proxyURL.Host)

			conn, err := p.dial("tcp", p.proxyURL.Host)

			if err != nil {
				return nil, nil, err
			}

			pbw := bufio.NewWriterSize(conn, DefaultWriteBufSize)
			pbr := bufio.NewReaderSize(conn, DefaultReadBufSize)

			req.Write(pbw)
			pbw.Flush()

			res, err := http.ReadResponse(pbr, req)
			if err != nil {
				return nil, nil, err
			}
			if res.StatusCode == 200 {
				return proxyutil.NewResponse(200, nil, req), conn, nil
			}
			return res, conn, nil
		}
	}

	log.Debugf("martian: CONNECT to host directly: %s", req.URL.Host)

	//conn, err := p.dial("tcp", req.URL.Host)
	var conn net.Conn
	if err := retry.Timed(MaxRetries, MaxRetryIntervalTime).On(func() error {
		conn, err = p.dial("tcp", req.URL.Host)
		if err != nil {
			return err
		}

		return nil
	}); err != nil {
		return nil, nil, err
	}
	//if err != nil {
	//	return nil, nil, err
	//}

	return proxyutil.NewResponse(200, nil, req), conn, nil
}

func (p *Proxy) getItemValue(item *badger.Item) (val []byte) {

	cm := "getItemValue@proxy.go"
	var v []byte
	err := item.Value(func(val []byte) error {
		v = val
		return nil
	})
	if err != nil {

		log.Errorf(cm+" error: %v", err)
	}
	if v == nil {
		return nil
	}
	return v
}
func (p *Proxy) inCh86cidr(ip net.IP) bool {

	cm := "inCh86cidr@proxy.go"

	resp, ok := p.Cache.Get(ip.String())

	if !ok {
		log.Errorf(cm+" not ok: %v, continue check ip if in cidr", ip.String())
	} else {

		if resp != "" {

			cr := resp == "true"
			log.Infof(cm+" cache hit: %v result: %v", ip.String(), cr)
			return cr
		}
	}

	for _, network := range Ch86cidr {

		_, subnet, _ := net.ParseCIDR(network)
		if subnet.Contains(ip) {
			p.Cache.Set(ip.String(), "true", 1)
			log.Infof("cache hit IP: %v in subnet: %v", ip, network)
			return true
		}
	}

	p.Cache.Set(ip.String(), "fasle", 1)
	log.Infof("IP: %v  not in ch86cidr", ip)
	return false
}

func (p *Proxy) SetCache(cache *ristretto.Cache) {
	p.Cache = cache
}
