package main

import (
	"crypto/tls"
	"flag"
	"github.com/dgraph-io/ristretto"
	"github.com/gohttpproxy/gohttpproxy/martian"
	"github.com/gohttpproxy/gohttpproxy/martian/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	addr    = flag.String("addr", "127.0.0.1:8080", "host:port of the proxy")
	lv      = flag.Int("lv", log.Debug, "default log level")
	h       = flag.Bool("h", false, "help")
	ds      = flag.String("ds", "", "down stream of the proxy")
	profile = flag.String("profile", "server", "choose client or server")
	sugar   *zap.SugaredLogger
)

func main() {
	cm := "main@main.go "
	atom := zap.NewAtomicLevel()

	// To keep the example deterministic, disable timestamps in the output.
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	logger := zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderCfg),
		zapcore.Lock(os.Stdout),
		atom,
	))
	defer logger.Sync()

	atom.SetLevel(zap.DebugLevel)

	sugar = logger.Sugar()
	flag.Parse()
	if *h {
		flag.PrintDefaults()
		os.Exit(0)
	}

	//设置默认级别
	log.SetLevel(*lv)
	//使用sugar为log
	log.SetLogger(sugar)

	log.Infof(" log level %v", *lv)

	go func() {
		sugar.Info(http.ListenAndServe("localhost:6062", nil))
	}()
	p := martian.NewProxy()

	// make cache

	Cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,     // number of keys to track frequency of (10M).
		MaxCost:     1 << 30, // maximum cost of cache (1GB).
		BufferItems: 64,      // number of keys per Get buffer.
	})
	if err != nil {
		panic(err)
	}
	p.SetCache(Cache)
	p.SetProfile(profile)
	//设置读写超时为30分钟，也就是10小时
	//	p.SetTimeout(6 * time.Second)
	defer p.Close()

	tr := &http.Transport{
		IdleConnTimeout:       90,
		ResponseHeaderTimeout: 3 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   -1,
		MaxConnsPerHost:       0,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	if *ds != "" {
		log.Infof(cm + " ds 不为空，开启下一层代理")

		u, err := url.Parse(*ds)
		if err != nil {
			log.Errorf(err.Error())
		}
		p.SetDownstreamProxy(u)
	}

	p.SetDial((&net.Dialer{
		KeepAlive: 30 * time.Second,
		Timeout:   3 * time.Second,
	}).Dial)
	p.SetRoundTripper(tr)

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Errorf(err.Error())
	}

	log.Infof("Starting proxy on : %s", l.Addr().String())

	go p.Serve(l)

	signChannel := make(chan os.Signal, 3)
	signal.Notify(signChannel, os.Interrupt, os.Kill, syscall.SIGTERM)

	<-signChannel

	log.Infof("Notice: shutting down")
	os.Exit(0)
}
