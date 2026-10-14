package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	url2 "net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beorn7/perks/quantile"
	"github.com/valyala/fasthttp"
	"golang.org/x/net/http/httpproxy"
)

// Request phases traced when --trace is enabled.
// DNS, Connect and TLS only happen when a new connection is established,
// Write, Wait (time to first byte) and Read happen on every request.
const (
	PhaseDNS = iota
	PhaseConnect
	PhaseTLS
	PhaseWrite
	PhaseWait
	PhaseRead
)

var phaseNames = [...]string{
	PhaseDNS:     "DNS",
	PhaseConnect: "Connect",
	PhaseTLS:     "TLS",
	PhaseWrite:   "Write",
	PhaseWait:    "Wait",
	PhaseRead:    "Read",
}

var phaseQuantiles = []float64{0.50, 0.90, 0.99}

// PhaseRecord carries the phases measured together: DNS/Connect/TLS of a new connection,
// or Write/Wait/Read of a request cycle. It's collected by StreamReport like ReportRecord.
type PhaseRecord struct {
	costs    [len(phaseNames)]time.Duration
	measured [len(phaseNames)]bool
}

func (pr *PhaseRecord) add(phase int, d time.Duration) {
	if d < 0 {
		d = 0
	}
	pr.costs[phase] = d
	pr.measured[phase] = true
}

// phaseStats is the phases part of StreamReport, guarded by StreamReport.lock.
// It's nil when --trace is disabled, all the methods are no-op on nil.
type phaseStats struct {
	stats     [len(phaseNames)]*Stats
	quantiles [len(phaseNames)]*quantile.Stream

	// per-second stats for the web charts
	withinSecTemp [len(phaseNames)]Stats
	withinSec     [len(phaseNames)]Stats
}

func newPhaseStats() *phaseStats {
	p := &phaseStats{}
	for i := range phaseNames {
		p.stats[i] = &Stats{}
		p.quantiles[i] = quantile.NewTargeted(quantilesTarget)
	}
	return p
}

func (p *phaseStats) insert(pr *PhaseRecord) {
	if p == nil {
		return
	}
	for i, ok := range pr.measured {
		if !ok {
			continue
		}
		v := float64(pr.costs[i])
		p.stats[i].Update(v)
		p.quantiles[i].Insert(v)
		p.withinSecTemp[i].Update(v)
	}
}

// rotate moves the stats of the past second to withinSec, it's called every second.
func (p *phaseStats) rotate() {
	if p == nil {
		return
	}
	p.withinSec = p.withinSecTemp
	for i := range p.withinSecTemp {
		p.withinSecTemp[i].Reset()
	}
}

// meansWithinSec returns the mean latency (ms) of each phase in the last second,
// nil for the phase which didn't happen.
func (p *phaseStats) meansWithinSec() []interface{} {
	if p == nil {
		return nil
	}
	res := make([]interface{}, len(phaseNames))
	for i := range p.withinSec {
		if p.withinSec[i].count > 0 {
			res[i] = p.withinSec[i].Mean() / 1e6
		}
	}
	return res
}

type PhaseSnapshot struct {
	Name        string
	Count       int64
	Min         time.Duration
	Mean        time.Duration
	Max         time.Duration
	Percentiles []time.Duration
}

func (p *phaseStats) snapshot() []*PhaseSnapshot {
	if p == nil {
		return nil
	}
	res := make([]*PhaseSnapshot, 0, len(phaseNames))
	for i := range phaseNames {
		s := p.stats[i]
		ps := &PhaseSnapshot{
			Name:  phaseNames[i],
			Count: s.count,
			Min:   time.Duration(s.min),
			Mean:  time.Duration(s.Mean()),
			Max:   time.Duration(s.max),
		}
		for _, q := range phaseQuantiles {
			ps.Percentiles = append(ps.Percentiles, time.Duration(p.quantiles[i].Query(q)))
		}
		res = append(res, ps)
	}
	return res
}

const phaseRecordsBuffer = 8192

// PhaseTracer delivers the PhaseRecords from the traced conns, like Requester.recordChan
// it's owned by Requester and closed together with recordChan.
// It's nil when --trace is disabled.
type PhaseTracer struct {
	records chan PhaseRecord

	// the senders include fasthttp's internal goroutines, so instead of recovering
	// from sending on the closed channel, the records sent after Close are dropped.
	closeLock sync.RWMutex
	closed    bool

	connsLock sync.Mutex
	conns     map[*traceConn]struct{}
}

func NewPhaseTracer() *PhaseTracer {
	return &PhaseTracer{
		records: make(chan PhaseRecord, phaseRecordsBuffer),
		conns:   make(map[*traceConn]struct{}),
	}
}

// Records returns nil when t is nil.
func (t *PhaseTracer) Records() <-chan PhaseRecord {
	if t == nil {
		return nil
	}
	return t.records
}

func (t *PhaseTracer) send(pr PhaseRecord) {
	t.closeLock.RLock()
	if !t.closed {
		t.records <- pr
	}
	t.closeLock.RUnlock()
}

// Close sends the last request cycle of every alive connection, which otherwise would be sent
// only when the connection is reused or closed, then closes the records channel.
func (t *PhaseTracer) Close() {
	if t == nil {
		return
	}
	t.connsLock.Lock()
	conns := make([]*traceConn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	t.connsLock.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		pr, ok := c.endCycleLocked()
		c.mu.Unlock()
		if ok {
			t.send(pr)
		}
	}

	t.closeLock.Lock()
	t.closed = true
	close(t.records)
	t.closeLock.Unlock()
}

// traceConn measures the request/response cycles on a keep-alive connection.
// A cycle starts at the first Write, the first successful Read marks the first
// response byte, and the cycle ends at the last Read before the next Write
// (HTTP/1.1 without pipelining), or when the connection/tracer is closed.
type traceConn struct {
	net.Conn
	tracer *PhaseTracer

	mu         sync.Mutex
	writing    bool
	reading    bool
	writeStart time.Time
	writeEnd   time.Time
	firstByte  time.Time
	lastRead   time.Time
}

func newTraceConn(conn net.Conn, tracer *PhaseTracer) *traceConn {
	c := &traceConn{Conn: conn, tracer: tracer}
	tracer.connsLock.Lock()
	tracer.conns[c] = struct{}{}
	tracer.connsLock.Unlock()
	return c
}

// endCycleLocked ends the current cycle, and returns it if the response has been read.
// The record is sent after releasing c.mu, so a full channel doesn't block the others waiting for c.mu.
func (c *traceConn) endCycleLocked() (pr PhaseRecord, ok bool) {
	if c.reading {
		pr.add(PhaseWrite, c.writeEnd.Sub(c.writeStart))
		pr.add(PhaseWait, c.firstByte.Sub(c.writeEnd))
		pr.add(PhaseRead, c.lastRead.Sub(c.firstByte))
		ok = true
	}
	c.writing = false
	c.reading = false
	return pr, ok
}

func (c *traceConn) Write(b []byte) (int, error) {
	// the network IO must be done without holding c.mu,
	// otherwise Close (which unblocks a stuck Write) would wait for the lock forever.
	start := time.Now()
	n, err := c.Conn.Write(b)
	end := time.Now()

	var pr PhaseRecord
	var ended bool
	c.mu.Lock()
	if !c.writing || c.reading {
		pr, ended = c.endCycleLocked()
		c.writing = true
		c.writeStart = start
	}
	c.writeEnd = end
	c.mu.Unlock()
	if ended {
		c.tracer.send(pr)
	}
	return n, err
}

func (c *traceConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		now := time.Now()
		c.mu.Lock()
		if c.writing && !c.reading {
			c.reading = true
			c.firstByte = now
		}
		if c.reading {
			c.lastRead = now
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *traceConn) Close() error {
	c.mu.Lock()
	pr, ended := c.endCycleLocked()
	c.mu.Unlock()
	if ended {
		c.tracer.send(pr)
	}
	c.tracer.connsLock.Lock()
	delete(c.tracer.conns, c)
	c.tracer.connsLock.Unlock()
	return c.Conn.Close()
}

// Handshake makes fasthttp treat the conn as an established TLS conn,
// since the handshake is already done (and measured) in TraceDial.
func (c *traceConn) Handshake() error {
	if hc, ok := c.Conn.(interface{ Handshake() error }); ok {
		return hc.Handshake()
	}
	return nil
}

func useProxy(opt *ClientOpt, addr string, isTLS bool) bool {
	if opt.socks5Proxy != "" || opt.httpProxy != "" {
		return true
	}
	scheme := "http"
	if isTLS {
		scheme = "https"
	}
	proxyURL, err := httpproxy.FromEnvironment().ProxyFunc()(&url2.URL{Scheme: scheme, Host: addr})
	return err == nil && proxyURL != nil
}

// TraceDial dials with DNS, TCP connect and TLS handshake measured separately.
// When wholeDial (the proxy or unix socket dialer) is not nil, the whole dial is measured as Connect,
// otherwise it dials TCP directly.
// The raw conn is wrapped by the throughput counter (r, w) before TLS,
// so throughput is still counted on the wire as without tracing.
func TraceDial(wholeDial fasthttp.DialFunc, isTLS bool, tlsConfig *tls.Config,
	dialTimeout, handshakeTimeout time.Duration, tracer *PhaseTracer, r, w *int64) fasthttp.DialFunc {
	if dialTimeout <= 0 {
		dialTimeout = fasthttp.DefaultDialTimeout
	}
	// shared by all connections to rotate the resolved IPs
	var ipIdx uint32
	return func(addr string) (net.Conn, error) {
		// the phases measured are sent even if the dial fails later
		var pr PhaseRecord
		defer func() { tracer.send(pr) }()

		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		var conn net.Conn
		if wholeDial != nil {
			start := time.Now()
			conn, err = wholeDial(addr)
			if err != nil {
				return nil, err
			}
			pr.add(PhaseConnect, time.Since(start))
		} else {
			conn, err = traceDialDirect(host, port, dialTimeout, &ipIdx, &pr)
			if err != nil {
				return nil, err
			}
		}

		conn, _ = NewMyConn(conn, r, w)

		if isTLS {
			cfg := tlsConfig.Clone()
			if cfg.ServerName == "" {
				cfg.ServerName = host
			}
			tlsConn := tls.Client(conn, cfg)
			if handshakeTimeout > 0 {
				_ = tlsConn.SetDeadline(time.Now().Add(handshakeTimeout))
			}
			start := time.Now()
			if err = tlsConn.Handshake(); err != nil {
				_ = conn.Close()
				return nil, err
			}
			pr.add(PhaseTLS, time.Since(start))
			_ = tlsConn.SetDeadline(time.Time{})
			conn = tlsConn
		}

		return newTraceConn(conn, tracer), nil
	}
}

// traceDialDirect resolves host and dials it as fasthttp's dual stack TCPDialer does:
// both IPv4 and IPv6, rotating the resolved IPs between dials and trying the next IP on failure,
// within dialTimeout in total.
func traceDialDirect(host, port string, dialTimeout time.Duration, ipIdx *uint32, pr *PhaseRecord) (net.Conn, error) {
	errDialTimeout := fmt.Errorf("error when dialing %s: %w", net.JoinHostPort(host, port), fasthttp.ErrDialTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()

	ips := []net.IPAddr{{IP: net.ParseIP(host)}}
	if ips[0].IP == nil {
		start := time.Now()
		var err error
		// LookupIPAddr keeps the zone of IPv6 link-local addresses
		ips, err = net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, errDialTimeout
			}
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("no such host: " + host)
		}
		pr.add(PhaseDNS, time.Since(start))
	}

	start := time.Now()
	idx := atomic.AddUint32(ipIdx, 1)
	n := uint32(len(ips))
	var d net.Dialer
	var err error
	for i := uint32(0); i < n; i++ {
		var conn net.Conn
		conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(ips[(idx+i)%n].String(), port))
		if err == nil {
			pr.add(PhaseConnect, time.Since(start))
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, errDialTimeout
		}
	}
	return nil, err
}
