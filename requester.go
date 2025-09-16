package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	url2 "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpproxy"
	"go.uber.org/automaxprocs/maxprocs"
	"golang.org/x/time/rate"
)

var (
	startTimeUnixNano int64
	sendOnCloseError  interface{}
)

type ReportRecord struct {
	cost             time.Duration
	code             int
	error            string
	readBytes        int64
	writeBytes       int64
	concurrencyCount int
}

var recordPool = sync.Pool{
	New: func() interface{} { return new(ReportRecord) },
}

func init() {
	// Honoring env GOMAXPROCS
	_, _ = maxprocs.Set()
	defer func() {
		sendOnCloseError = recover()
	}()
	func() {
		cc := make(chan struct{}, 1)
		close(cc)
		cc <- struct{}{}
	}()
}

type MyConn struct {
	net.Conn
	r, w *int64
}

func NewMyConn(conn net.Conn, r, w *int64) (*MyConn, error) {
	myConn := &MyConn{Conn: conn, r: r, w: w}
	return myConn, nil
}

func (c *MyConn) Read(b []byte) (n int, err error) {
	sz, err := c.Conn.Read(b)

	if err == nil {
		atomic.AddInt64(c.r, int64(sz))
	}
	return sz, err
}

func (c *MyConn) Write(b []byte) (n int, err error) {
	sz, err := c.Conn.Write(b)

	if err == nil {
		atomic.AddInt64(c.w, int64(sz))
	}
	return sz, err
}

func ThroughputInterceptorDial(dial fasthttp.DialFunc, r *int64, w *int64) fasthttp.DialFunc {
	return func(addr string) (net.Conn, error) {
		conn, err := dial(addr)
		if err != nil {
			return nil, err
		}
		return NewMyConn(conn, r, w)
	}
}

type Requester struct {
	concurrency int
	reqRate     *rate.Limit
	requests    int64
	duration    time.Duration
	rampUp      int
	clientOpt   *ClientOpt
	httpClient  *fasthttp.HostClient
	httpHeader  *fasthttp.RequestHeader
	errWriter   io.Writer

	recordChan chan *ReportRecord
	closeOnce  sync.Once
	wg         sync.WaitGroup

	readBytes  int64
	writeBytes int64

	cancel func()
}

type ClientOpt struct {
	url       string
	method    string
	headers   []string
	bodyBytes []byte
	bodyFile  string

	certPath string
	keyPath  string
	insecure bool

	maxConns     int
	doTimeout    time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration
	dialTimeout  time.Duration

	socks5Proxy string
	contentType string
	host        string
	unixSocket  string
}

func NewRequester(concurrency int, requests int64, duration time.Duration, reqRate *rate.Limit, errWriter io.Writer, clientOpt *ClientOpt, rampUp int) (*Requester, error) {
	maxResult := concurrency * 100
	if maxResult > 8192 {
		maxResult = 8192
	}
	r := &Requester{
		concurrency: concurrency,
		reqRate:     reqRate,
		requests:    requests,
		duration:    duration,
		rampUp:      rampUp,
		errWriter:   errWriter,
		clientOpt:   clientOpt,
		recordChan:  make(chan *ReportRecord, maxResult),
	}
	client, header, err := buildRequestClient(clientOpt, &r.readBytes, &r.writeBytes)
	if err != nil {
		return nil, err
	}
	r.httpClient = client
	r.httpHeader = header
	return r, nil
}

func addMissingPort(addr string, isTLS bool) string {
	n := strings.Index(addr, ":")
	if n >= 0 {
		return addr
	}
	port := 80
	if isTLS {
		port = 443
	}
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

func buildTLSConfig(opt *ClientOpt) (*tls.Config, error) {
	var certs []tls.Certificate
	if opt.certPath != "" && opt.keyPath != "" {
		c, err := tls.LoadX509KeyPair(opt.certPath, opt.keyPath)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	return &tls.Config{
		InsecureSkipVerify: opt.insecure,
		Certificates:       certs,
	}, nil
}

func buildRequestClient(opt *ClientOpt, r *int64, w *int64) (*fasthttp.HostClient, *fasthttp.RequestHeader, error) {
	u, err := url2.Parse(opt.url)
	if err != nil {
		return nil, nil, err
	}
	httpClient := &fasthttp.HostClient{
		Addr:                          addMissingPort(u.Host, u.Scheme == "https"),
		IsTLS:                         u.Scheme == "https",
		Name:                          "plow",
		MaxConns:                      opt.maxConns,
		ReadTimeout:                   opt.readTimeout,
		WriteTimeout:                  opt.writeTimeout,
		DisableHeaderNamesNormalizing: true,
	}
	if opt.socks5Proxy != "" {
		if !strings.Contains(opt.socks5Proxy, "://") {
			opt.socks5Proxy = "socks5://" + opt.socks5Proxy
		}
		httpClient.Dial = fasthttpproxy.FasthttpSocksDialer(opt.socks5Proxy)
	} else if opt.unixSocket != "" {
		httpClient.Dial = func(addr string) (net.Conn, error) {
			return net.Dial("unix", opt.unixSocket)
		}
	} else {
		httpClient.Dial = fasthttpproxy.FasthttpProxyHTTPDialerTimeout(opt.dialTimeout)
	}
	httpClient.Dial = ThroughputInterceptorDial(httpClient.Dial, r, w)

	tlsConfig, err := buildTLSConfig(opt)
	if err != nil {
		return nil, nil, err
	}
	httpClient.TLSConfig = tlsConfig

	var requestHeader fasthttp.RequestHeader
	if opt.contentType != "" {
		requestHeader.SetContentType(opt.contentType)
	}
	if opt.host != "" {
		requestHeader.SetHost(opt.host)
	} else {
		requestHeader.SetHost(u.Host)
	}
	requestHeader.SetMethod(opt.method)
	requestHeader.SetRequestURI(u.RequestURI())
	for _, h := range opt.headers {
		n := strings.SplitN(h, ":", 2)
		if len(n) != 2 {
			return nil, nil, fmt.Errorf("invalid header: %s", h)
		}
		requestHeader.Set(n[0], n[1])
	}

	return httpClient, &requestHeader, nil
}

func (r *Requester) Cancel() {
	r.cancel()
}

func (r *Requester) RecordChan() <-chan *ReportRecord {
	return r.recordChan
}

func (r *Requester) closeRecord() {
	r.closeOnce.Do(func() {
		close(r.recordChan)
	})
}

func (r *Requester) DoRequest(req *fasthttp.Request, resp *fasthttp.Response, rr *ReportRecord) {
	startTime := time.Unix(0, atomic.LoadInt64(&startTimeUnixNano))
	t1 := time.Since(startTime)
	var err error
	if r.clientOpt.doTimeout > 0 {
		err = r.httpClient.DoTimeout(req, resp, r.clientOpt.doTimeout)
	} else {
		err = r.httpClient.Do(req, resp)
	}

	if err != nil {
		rr.cost = time.Since(startTime) - t1
		rr.error = err.Error()
		return
	}

	writeTo := io.Discard
	if resp.StatusCode() >= 500 {
		writeTo = r.errWriter
		_, _ = r.errWriter.Write([]byte(fmt.Sprintf("\n%d %s\n", resp.StatusCode(), rr.cost)))
		_, _ = r.errWriter.Write([]byte(fmt.Sprintf("%s", &resp.Header)))
	}
	err = resp.BodyWriteTo(writeTo)
	if err != nil {
		rr.cost = time.Since(startTime) - t1
		rr.error = err.Error()
		return
	}

	rr.cost = time.Since(startTime) - t1
	rr.code = resp.StatusCode()
	rr.error = ""
}

func (r *Requester) Run() {
	// handle ctrl-c
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	ctx, cancelFunc := context.WithCancel(context.Background())
	r.cancel = cancelFunc
	go func() {
		<-sigs
		r.closeRecord()
		cancelFunc()
	}()
	atomic.StoreInt64(&startTimeUnixNano, time.Now().UnixNano())
	if r.duration > 0 {
		time.AfterFunc(r.duration, func() {
			r.closeRecord()
			cancelFunc()
		})
	}

	var limiter *rate.Limiter
	if r.reqRate != nil {
		limiter = rate.NewLimiter(*r.reqRate, 1)
	}

	semaphore := r.requests
	if r.rampUp <= 0 {
		r.rampUp = r.concurrency
	}
	concurrencyCount := 0
	loopCount := int(math.Ceil(float64(r.concurrency) / float64(r.rampUp)))
	for i := 0; i < loopCount; i++ {
		for j := 0; j < r.rampUp; j++ {
			if concurrencyCount > r.concurrency {
				break
			}
			concurrencyCount++
			r.wg.Add(1)
			go func() {
				defer func() {
					r.wg.Done()
					v := recover()
					if v != nil && v != sendOnCloseError {
						panic(v)
					}
				}()
				req := &fasthttp.Request{}
				resp := &fasthttp.Response{}
				r.httpHeader.CopyTo(&req.Header)
				if r.httpClient.IsTLS {
					req.URI().SetScheme("https")
					req.URI().SetHostBytes(req.Header.Host())
				}

				for {
					select {
					case <-ctx.Done():
						return
					default:
					}

					if limiter != nil {
						err := limiter.Wait(ctx)
						if err != nil {
							continue
						}
					}

					if r.requests > 0 && atomic.AddInt64(&semaphore, -1) < 0 {
						cancelFunc()
						return
					}

					if r.clientOpt.bodyFile != "" {
						file, err := os.Open(r.clientOpt.bodyFile)
						if err != nil {
							rr := recordPool.Get().(*ReportRecord)
							rr.cost = 0
							rr.error = err.Error()
							rr.readBytes = atomic.LoadInt64(&r.readBytes)
							rr.writeBytes = atomic.LoadInt64(&r.writeBytes)
							rr.concurrencyCount = concurrencyCount
							r.recordChan <- rr
							continue
						}
						req.SetBodyStream(file, -1)
					} else {
						req.SetBodyRaw(r.clientOpt.bodyBytes)
					}
					resp.Reset()
					rr := recordPool.Get().(*ReportRecord)
					r.DoRequest(req, resp, rr)
					rr.readBytes = atomic.LoadInt64(&r.readBytes)
					rr.writeBytes = atomic.LoadInt64(&r.writeBytes)
					rr.concurrencyCount = concurrencyCount
					r.recordChan <- rr
				}
			}()
		}
		if r.rampUp != r.concurrency {
			time.Sleep(time.Second)
		}
	}

	r.wg.Wait()
	r.closeRecord()
}
