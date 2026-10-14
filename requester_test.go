package main

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestAddMissingPort(t *testing.T) {
	tests := []struct {
		name string
		addr string
		tls  bool
		want string
	}{
		{name: "host without port", addr: "example.com", want: "example.com:80"},
		{name: "host with port", addr: "example.com:8080", want: "example.com:8080"},
		{name: "ipv6 without port", addr: "[::1]", want: "[::1]:80"},
		{name: "ipv6 with port", addr: "[::1]:8080", want: "[::1]:8080"},
		{name: "https default", addr: "example.com", tls: true, want: "example.com:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := addMissingPort(tt.addr, tt.tls)
			if got != tt.want {
				t.Fatalf("addMissingPort(%q, %v) = %q, want %q", tt.addr, tt.tls, got, tt.want)
			}
		})
	}
}

func TestBuildRequestClientUsesDualStackDialer(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("no_proxy", "*")

	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is not available: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		_ = fasthttp.Serve(ln, func(ctx *fasthttp.RequestCtx) {
			ctx.SetStatusCode(fasthttp.StatusOK)
		})
		close(done)
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"localhost", "[::1]"} {
		opt := &ClientOpt{
			url:         "http://" + target + ":" + port + "/",
			method:      fasthttp.MethodGet,
			maxConns:    1,
			dialTimeout: time.Second,
			doTimeout:   time.Second,
		}
		client, header, err := buildRequestClient(opt, new(int64), new(int64), nil)
		if err != nil {
			t.Fatal(err)
		}

		var req fasthttp.Request
		var resp fasthttp.Response
		header.CopyTo(&req.Header)
		if err := client.DoTimeout(&req, &resp, time.Second); err != nil {
			t.Fatalf("request to %s failed: %v", opt.url, err)
		}
		if resp.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("request to %s returned status %d", opt.url, resp.StatusCode())
		}
	}

	_ = ln.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestBuildRequestClientUsesHTTPProxy(t *testing.T) {
	proxyAddr, proxyHits := startTestProxy(t)
	proxyURL := "http://" + proxyAddr

	tests := []struct {
		name      string
		envProxy  bool
		httpProxy string
	}{
		{name: "environment proxy", envProxy: true},
		{name: "explicit proxy", httpProxy: proxyURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HTTP_PROXY", "")
			t.Setenv("HTTPS_PROXY", "")
			t.Setenv("NO_PROXY", "")
			t.Setenv("http_proxy", "")
			t.Setenv("https_proxy", "")
			t.Setenv("no_proxy", "")
			if tt.envProxy {
				t.Setenv("HTTP_PROXY", proxyURL)
			}

			before := atomic.LoadInt64(proxyHits)
			opt := &ClientOpt{
				url:         "http://proxy-target.invalid/",
				method:      fasthttp.MethodGet,
				maxConns:    1,
				dialTimeout: time.Second,
				doTimeout:   time.Second,
				httpProxy:   tt.httpProxy,
			}
			client, header, err := buildRequestClient(opt, new(int64), new(int64), nil)
			if err != nil {
				t.Fatal(err)
			}

			var req fasthttp.Request
			var resp fasthttp.Response
			header.CopyTo(&req.Header)
			if err := client.DoTimeout(&req, &resp, time.Second); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode(), fasthttp.StatusOK)
			}
			if got := atomic.LoadInt64(proxyHits) - before; got == 0 {
				t.Fatal("proxy was not used")
			}
		})
	}
}

func startTestProxy(t *testing.T) (string, *int64) {
	t.Helper()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var hits int64
	done := make(chan struct{})
	go func() {
		_ = fasthttp.Serve(ln, func(ctx *fasthttp.RequestCtx) {
			atomic.AddInt64(&hits, 1)
			ctx.SetStatusCode(fasthttp.StatusOK)
		})
		close(done)
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("proxy server did not stop")
		}
	})

	return ln.Addr().String(), &hits
}
