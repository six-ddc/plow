package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"gopkg.in/alecthomas/kingpin.v3-unstable"
)

func newTestSnapshot() *SnapshotReport {
	return &SnapshotReport{
		Elapsed: 2 * time.Second,
		Count:   100,
		RPS:     50,
		Codes:   map[string]int64{"2xx": 90, "5xx": 10},
		Errors:  map[string]int64{},
		Stats: &struct {
			Min    time.Duration
			Mean   time.Duration
			StdDev time.Duration
			Max    time.Duration
		}{time.Millisecond, 2 * time.Millisecond, time.Millisecond, 9 * time.Millisecond},
		Percentiles: []*struct {
			Percentile float64
			Latency    time.Duration
		}{{0.5, 2 * time.Millisecond}, {0.99, 8 * time.Millisecond}},
		Phases: []*PhaseSnapshot{{
			Name:        "Connect",
			Count:       3,
			Mean:        time.Millisecond,
			Percentiles: []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond},
		}},
	}
}

func TestHistorySave(t *testing.T) {
	history, err := NewHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seconds := make(chan SeriesPoint, 1)
	seconds <- SeriesPoint{Elapsed: time.Second, Count: 100}
	close(seconds)
	history.Record(seconds)

	opts := HistoryOptions{URL: "http://example.com/", Method: "GET", Concurrency: 4, Duration: 2 * time.Second, Trace: true}
	if _, err = history.Save(&HistoryRun{Interrupted: true, Label: "x", Options: opts}, newTestSnapshot()); err != nil {
		t.Fatal(err)
	}
	runs, err := history.List()
	if err != nil || len(runs) != 1 {
		t.Fatalf("List() = %v, %v, want one run", runs, err)
	}
	run := runs[0]
	if run.Version != historyVersion || !run.Interrupted || run.Label != "x" || run.Options != opts || run.Time.IsZero() {
		t.Fatalf("run = %+v, want version %d, interrupted, label and options %+v", run, historyVersion, opts)
	}
	if len(run.Series) != 1 || run.Series[0].Count != 100 {
		t.Fatalf("series = %+v, want the recorded point", run.Series)
	}
	res := run.Result
	if res.Count != 100 || res.RPS != 50 || res.Codes["5xx"] != 10 {
		t.Fatalf("result = %+v, want count 100, rps 50 and 10 5xx", res)
	}
	if res.Latency.Mean != 2*time.Millisecond || res.Latency.Max != 9*time.Millisecond {
		t.Fatalf("latency = %+v, want mean 2ms and max 9ms", res.Latency)
	}
	if len(res.Percentiles) != 2 || res.Percentiles[1] != (HistoryPercentile{0.99, 8 * time.Millisecond}) {
		t.Fatalf("percentiles = %+v, want P99 8ms", res.Percentiles)
	}
	if len(res.Phases) != 1 || res.Phases[0].Name != "Connect" || len(res.Phases[0].Percentiles) != len(phaseQuantiles) {
		t.Fatalf("phases = %+v, want Connect with %d percentiles", res.Phases, len(phaseQuantiles))
	}
	if p := res.Phases[0].Percentiles[2]; p.Percentile != 0.99 || p.Latency != 3*time.Millisecond {
		t.Fatalf("Connect P99 = %+v, want 3ms", p)
	}
}

func TestHistorySaveAndList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	history, err := NewHistory(dir)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := history.List()
	if err != nil || len(runs) != 0 {
		t.Fatalf("List() on missing dir = %v, %v, want empty", runs, err)
	}

	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	for i, url := range []string{"http://a/", "http://b/", "http://c/"} {
		run := &HistoryRun{Time: base.Add(time.Duration(i) * time.Minute), Options: HistoryOptions{URL: url, Method: "GET"}}
		path, err := history.Save(run, newTestSnapshot())
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(path) != run.ID+".json" || !strings.HasPrefix(run.ID, run.Time.Format("20060102-150405")) || !historyIDPattern.MatchString(run.ID) {
			t.Fatalf("saved to %s with ID %s, want the ID named by the time", path, run.ID)
		}
	}
	// the unparsable, other versions and non-json files are skipped
	_ = os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "future.json"), []byte(`{"Version": 99}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644)

	runs, err = history.List()
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, r := range runs {
		urls = append(urls, r.Options.URL)
	}
	if strings.Join(urls, ",") != "http://c/,http://b/,http://a/" {
		t.Fatalf("List() urls = %v, want newest first", urls)
	}
	if got := runs[0].Result.Percentiles[1].Latency; got != 8*time.Millisecond {
		t.Fatalf("loaded P99 = %s, want 8ms", got)
	}
}

func TestServeHistoryAPI(t *testing.T) {
	history, err := NewHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := &HistoryRun{Options: HistoryOptions{URL: "http://a/"}}
	if _, err = history.Save(run, newTestSnapshot()); err != nil {
		t.Fatal(err)
	}

	serve := func(history *History, method, path string) (bool, *fasthttp.Response) {
		var req fasthttp.Request
		var ctx fasthttp.RequestCtx
		req.Header.SetMethod(method)
		req.SetRequestURI(path)
		ctx.Init(&req, nil, nil)
		return serveHistoryAPI(&ctx, history), &ctx.Response
	}

	ok, resp := serve(history, "GET", historyAPIPath)
	var runs []*HistoryRun
	if err := json.Unmarshal(resp.Body(), &runs); !ok || err != nil || len(runs) != 1 || runs[0].Options.URL != "http://a/" {
		t.Fatalf("history api = %v, %v, %s", ok, err, resp.Body())
	}

	if _, resp := serve(history, "DELETE", historyAPIPath+"/bad-id"); resp.StatusCode() != fasthttp.StatusBadRequest {
		t.Fatalf("DELETE bad id status = %d, want 400", resp.StatusCode())
	}
	if _, resp := serve(history, "DELETE", historyAPIPath+"/20000101-000000-0000"); resp.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("DELETE missing run status = %d, want 404", resp.StatusCode())
	}
	if ok, _ := serve(history, "GET", historyAPIPath+"/"+run.ID); ok {
		t.Fatal("GET a run path is handled, want only DELETE")
	}
	if _, resp := serve(history, "DELETE", historyAPIPath+"/"+run.ID); resp.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("DELETE run status = %d, want 200", resp.StatusCode())
	}
	if runs, _ := history.List(); len(runs) != 0 {
		t.Fatalf("runs after DELETE = %d, want 0", len(runs))
	}

	if ok, _ := serve(history, "GET", "/"); ok {
		t.Fatal("serveHistoryAPI handled /, want false")
	}
}

func TestHistoryDelete(t *testing.T) {
	dir := t.TempDir()
	history, err := NewHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "..", "outside.json")
	_ = os.WriteFile(outside, []byte("{}"), 0o644)
	defer os.Remove(outside)

	for _, id := range []string{"", "../outside", "20261020-100000-abcd/../../outside", "20261020-100000-ABCD"} {
		if err := history.Delete(id); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Delete(%q) = %v, want invalid id error", id, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("file outside the history dir is removed: %v", err)
	}
	if err := history.Delete("20261020-100000-abcd"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Delete(missing) = %v, want ErrNotExist", err)
	}
}

func TestParseHistoryFlags(t *testing.T) {
	parse := func(args ...string) {
		t.Helper()
		if _, err := kingpin.CommandLine.Parse(args); err != nil {
			t.Fatalf("Parse(%v) error: %v", args, err)
		}
	}

	parse("http://example.com/", "-c", "3")
	if *url != "http://example.com/" || *concurrency != 3 || *recordHistory {
		t.Fatalf("url %q concurrency %d history %v, want http://example.com/ 3 false", *url, *concurrency, *recordHistory)
	}
	parse("http://example.com/", "--history", "--history-dir", "/tmp/h", "--label", "after cache")
	if !*recordHistory || *historyDir != "/tmp/h" || *label != "after cache" {
		t.Fatalf("history %v dir %q label %q, want true /tmp/h after cache", *recordHistory, *historyDir, *label)
	}
}

func TestRequestFingerprint(t *testing.T) {
	base := ClientOpt{headers: []string{"A: 1", "B: 2"}, bodyBytes: []byte("hello"), host: "h", doTimeout: time.Second}
	same := base
	same.headers = []string{"B: 2", "A: 1"}
	if requestFingerprint(&base) != requestFingerprint(&same) {
		t.Fatal("fingerprint depends on the order of the headers")
	}
	for name, change := range map[string]func(o *ClientOpt){
		"body":    func(o *ClientOpt) { o.bodyBytes = []byte("hello!") },
		"header":  func(o *ClientOpt) { o.headers = []string{"A: 1", "B: 3"} },
		"host":    func(o *ClientOpt) { o.host = "other" },
		"timeout": func(o *ClientOpt) { o.doTimeout = 2 * time.Second },
		"proxy":   func(o *ClientOpt) { o.httpProxy = "user:secret@proxy:8080" },
	} {
		changed := base
		change(&changed)
		fp := requestFingerprint(&changed)
		if fp == requestFingerprint(&base) {
			t.Fatalf("fingerprint doesn't change with the %s", name)
		}
		if len(fp) != 16 || strings.Contains(fp, "secret") {
			t.Fatalf("fingerprint %q, want 16 hex digits", fp)
		}
	}
}

func TestHistoryHandler(t *testing.T) {
	history, err := NewHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seconds := make(chan SeriesPoint, 1)
	seconds <- SeriesPoint{Elapsed: time.Second, Count: 100, RPS: 100, Mean: time.Millisecond}
	close(seconds)
	history.Record(seconds)
	if _, err = history.Save(&HistoryRun{Options: HistoryOptions{URL: "http://a/", Method: "GET"}}, newTestSnapshot()); err != nil {
		t.Fatal(err)
	}

	handler := HistoryHandler(history)
	get := func(path string) *fasthttp.Response {
		var req fasthttp.Request
		var ctx fasthttp.RequestCtx
		req.SetRequestURI(path)
		ctx.Init(&req, nil, nil)
		handler(&ctx)
		return &ctx.Response
	}

	page := string(get("/").Body())
	for _, want := range []string{assetsPath + "echarts.min.js", ".plow-history", "function PlowHistory", "PlowHistory(document"} {
		if !strings.Contains(page, want) {
			t.Fatalf("history page doesn't contain %q:\n%s", want, page)
		}
	}
	if resp := get(assetsPath + "echarts.min.js"); resp.StatusCode() != fasthttp.StatusOK {
		t.Fatalf("echarts status = %d, want 200", resp.StatusCode())
	}
	var runs []*HistoryRun
	if err := json.Unmarshal(get(historyAPIPath).Body(), &runs); err != nil || len(runs) != 1 || len(runs[0].Series) != 1 || runs[0].Series[0].RPS != 100 {
		t.Fatalf("history api = %+v, %v, want the run with its series", runs, err)
	}
	if resp := get("/missing"); resp.StatusCode() != fasthttp.StatusNotFound {
		t.Fatalf("missing path status = %d, want 404", resp.StatusCode())
	}
}
