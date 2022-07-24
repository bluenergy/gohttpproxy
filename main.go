package main

import (
	"crypto/tls"
	"flag"
	"github.com/cnmade/martian/v3"
	"github.com/cnmade/martian/v3/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var (
	addr  = flag.String("addr", "127.0.0.1:8080", "host:port of the proxy")
	lv    = flag.Int("lv", log.Debug, "default log level")
	h     = flag.Bool("h", false, "help")
	sugar *zap.SugaredLogger
)

func main() {
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

	martian.DefaultProxyIdleTimeout = 20 * time.Second

	go func() {
		sugar.Info(http.ListenAndServe("localhost:6062", nil))
	}()
	p := martian.NewProxy()
	//设置读写超时为30分钟，也就是10小时
	//	p.SetTimeout(6 * time.Second)
	defer p.Close()

	tr := &http.Transport{
		IdleConnTimeout:       0,
		ResponseHeaderTimeout: 3 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 3 * time.Second,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
		MaxConnsPerHost:       4096,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	p.SetDial((&net.Dialer{
		KeepAlive: 15 * time.Second,
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
