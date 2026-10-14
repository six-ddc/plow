package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

func TestBuildRequestClientConfiguresRequestHeader(t *testing.T) {
	opt := &ClientOpt{
		url:         "http://example.com:8080/api/v1?q=go",
		method:      fasthttp.MethodPatch,
		maxConns:    7,
		contentType: "application/json",
		host:        "override.example",
		headers:     []string{" X-Test : friendly \t", "\tX-Trace-ID\t: abc123 "},
	}

	client, header, err := buildRequestClient(opt, new(int64), new(int64), nil)
	if err != nil {
		t.Fatal(err)
	}

	if client.Addr != "example.com:8080" {
		t.Fatalf("client.Addr = %q, want example.com:8080", client.Addr)
	}
	if client.IsTLS {
		t.Fatal("client.IsTLS = true, want false")
	}
	if client.MaxConns != 7 {
		t.Fatalf("client.MaxConns = %d, want 7", client.MaxConns)
	}
	if got := string(header.Method()); got != fasthttp.MethodPatch {
		t.Fatalf("method = %q, want PATCH", got)
	}
	if got := string(header.Host()); got != "override.example" {
		t.Fatalf("host = %q, want override.example", got)
	}
	if got := string(header.ContentType()); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := string(header.Peek("X-Test")); got != "friendly" {
		t.Fatalf("X-Test = %q, want friendly", got)
	}
	if got := string(header.Peek("X-Trace-ID")); got != "abc123" {
		t.Fatalf("X-Trace-ID = %q, want abc123", got)
	}
	if got := string(header.RequestURI()); got != "/api/v1?q=go" {
		t.Fatalf("request URI = %q, want /api/v1?q=go", got)
	}
}

func TestBuildRequestClientConfiguresTLS(t *testing.T) {
	client, header, err := buildRequestClient(&ClientOpt{
		url:      "https://example.com/secure",
		method:   fasthttp.MethodGet,
		maxConns: 1,
		insecure: true,
	}, new(int64), new(int64), nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.Addr != "example.com:443" {
		t.Fatalf("client.Addr = %q, want example.com:443", client.Addr)
	}
	if !client.IsTLS {
		t.Fatal("client.IsTLS = false, want true")
	}
	if client.TLSConfig == nil || !client.TLSConfig.InsecureSkipVerify {
		t.Fatalf("TLSConfig = %+v, want InsecureSkipVerify", client.TLSConfig)
	}
	if got := string(header.Host()); got != "example.com" {
		t.Fatalf("host = %q, want example.com", got)
	}
}

func TestBuildRequestClientRejectsInvalidHeader(t *testing.T) {
	_, _, err := buildRequestClient(&ClientOpt{
		url:      "http://example.com/",
		method:   fasthttp.MethodGet,
		maxConns: 1,
		headers:  []string{"MissingColon"},
	}, new(int64), new(int64), nil)
	if err == nil {
		t.Fatal("buildRequestClient accepted an invalid header, want error")
	}
}

func TestBuildRequestClientUsesUnixSocket(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("plow-%d.sock", time.Now().UnixNano()))
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_ = fasthttp.Serve(ln, func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) != "/unix" {
				ctx.SetStatusCode(fasthttp.StatusNotFound)
				return
			}
			ctx.SetStatusCode(fasthttp.StatusAccepted)
		})
		close(done)
	}()
	defer func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("unix socket server did not stop")
		}
	}()

	client, header, err := buildRequestClient(&ClientOpt{
		url:         "http://unix.example/unix",
		method:      fasthttp.MethodGet,
		maxConns:    1,
		dialTimeout: time.Second,
		doTimeout:   time.Second,
		unixSocket:  socketPath,
	}, new(int64), new(int64), nil)
	if err != nil {
		t.Fatal(err)
	}

	var req fasthttp.Request
	var resp fasthttp.Response
	header.CopyTo(&req.Header)
	if err := client.DoTimeout(&req, &resp, time.Second); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != fasthttp.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode(), fasthttp.StatusAccepted)
	}
}

func TestRequesterRunSendsConfiguredRequests(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var hits int64
	var mismatches int64
	done := make(chan struct{})
	go func() {
		_ = fasthttp.Serve(ln, func(ctx *fasthttp.RequestCtx) {
			atomic.AddInt64(&hits, 1)
			if string(ctx.Method()) != fasthttp.MethodPost ||
				string(ctx.Path()) != "/submit" ||
				string(ctx.QueryArgs().Peek("token")) != "1" ||
				string(ctx.Request.Header.Peek("X-Test")) != "friendly" ||
				string(ctx.Request.Header.ContentType()) != "text/plain" ||
				!bytes.Equal(ctx.PostBody(), []byte("hello")) {
				atomic.AddInt64(&mismatches, 1)
				ctx.SetStatusCode(fasthttp.StatusTeapot)
				return
			}
			ctx.SetStatusCode(fasthttp.StatusCreated)
			ctx.SetBodyString("ok")
		})
		close(done)
	}()
	defer func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("test server did not stop")
		}
	}()

	var errOut bytes.Buffer
	requester, err := NewRequester(2, 4, 0, nil, &errOut, &ClientOpt{
		url:         "http://" + ln.Addr().String() + "/submit?token=1",
		method:      fasthttp.MethodPost,
		headers:     []string{"X-Test: friendly"},
		bodyBytes:   []byte("hello"),
		contentType: "text/plain",
		maxConns:    2,
		dialTimeout: time.Second,
		doTimeout:   time.Second,
	}, -1, false)
	if err != nil {
		t.Fatal(err)
	}

	requester.Run()

	var records []*ReportRecord
	for record := range requester.RecordChan() {
		records = append(records, record)
	}
	if len(records) != 4 {
		t.Fatalf("records len = %d, want 4", len(records))
	}
	if got := atomic.LoadInt64(&hits); got != 4 {
		t.Fatalf("server hits = %d, want 4", got)
	}
	if got := atomic.LoadInt64(&mismatches); got != 0 {
		t.Fatalf("server saw %d malformed request(s)", got)
	}
	for i, record := range records {
		if record.error != "" || record.code != fasthttp.StatusCreated {
			t.Fatalf("record[%d] = code %d error %q, want 201 with no error", i, record.code, record.error)
		}
		if record.cost <= 0 {
			t.Fatalf("record[%d].cost = %s, want positive", i, record.cost)
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("error output = %q, want empty", errOut.String())
	}
}
