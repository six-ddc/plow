package main

import (
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatsUpdateMeanStddevAndReset(t *testing.T) {
	var s Stats
	for _, v := range []float64{1, 2, 3} {
		s.Update(v)
	}

	if s.count != 3 {
		t.Fatalf("count = %d, want 3", s.count)
	}
	if s.min != 1 || s.max != 3 {
		t.Fatalf("min/max = %v/%v, want 1/3", s.min, s.max)
	}
	if got := s.Mean(); got != 2 {
		t.Fatalf("Mean() = %v, want 2", got)
	}
	if got := s.Stddev(); math.Abs(got-1) > 1e-9 {
		t.Fatalf("Stddev() = %v, want 1", got)
	}

	s.Reset()
	if s.count != 0 || s.sum != 0 || s.sumSq != 0 || s.min != 0 || s.max != 0 {
		t.Fatalf("Reset() left stats as %+v, want zero value", s)
	}
	if got := s.Mean(); got != 0 {
		t.Fatalf("Mean() after Reset() = %v, want 0", got)
	}
	if got := s.Stddev(); got != 0 {
		t.Fatalf("Stddev() after Reset() = %v, want 0", got)
	}
}

func TestStreamReportCollectAndSnapshot(t *testing.T) {
	oldStartTime := atomic.LoadInt64(&startTimeUnixNano)
	t.Cleanup(func() { atomic.StoreInt64(&startTimeUnixNano, oldStartTime) })
	atomic.StoreInt64(&startTimeUnixNano, time.Now().Add(-2*time.Second).UnixNano())

	report := NewStreamReport(false)
	records := make(chan *ReportRecord, 3)
	done := make(chan struct{})
	go func() {
		report.Collect(records, nil)
		close(done)
	}()

	records <- &ReportRecord{cost: 10 * time.Millisecond, code: 200, readBytes: 100, writeBytes: 50, concurrencyCount: 1}
	records <- &ReportRecord{cost: 30 * time.Millisecond, code: 503, error: "backend exploded", readBytes: 300, writeBytes: 80, concurrencyCount: 2}
	records <- &ReportRecord{cost: 20 * time.Millisecond, code: 404, readBytes: 400, writeBytes: 100, concurrencyCount: 2}
	close(records)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Collect did not stop after records channel was closed")
	}

	snapshot := report.Snapshot()
	if snapshot.Count != 3 {
		t.Fatalf("Count = %d, want 3", snapshot.Count)
	}
	if snapshot.Codes["2xx"] != 1 || snapshot.Codes["4xx"] != 1 || snapshot.Codes["5xx"] != 1 {
		t.Fatalf("Codes = %#v, want one 2xx, one 4xx and one 5xx", snapshot.Codes)
	}
	if snapshot.Errors["backend exploded"] != 1 {
		t.Fatalf("Errors = %#v, want backend exploded once", snapshot.Errors)
	}
	if snapshot.Stats.Min != 10*time.Millisecond || snapshot.Stats.Max != 30*time.Millisecond || snapshot.Stats.Mean != 20*time.Millisecond {
		t.Fatalf("latency stats = %+v, want min 10ms, mean 20ms, max 30ms", snapshot.Stats)
	}
	if snapshot.concurrencyCount != 2 {
		t.Fatalf("concurrencyCount = %d, want 2", snapshot.concurrencyCount)
	}
	if snapshot.ReadThroughput <= 0 || snapshot.WriteThroughput <= 0 {
		t.Fatalf("throughput = read %v write %v, want both positive", snapshot.ReadThroughput, snapshot.WriteThroughput)
	}
	if len(snapshot.Percentiles) != len(quantiles) {
		t.Fatalf("Percentiles len = %d, want %d", len(snapshot.Percentiles), len(quantiles))
	}

	var histogramCount int
	for _, bin := range snapshot.Histograms {
		histogramCount += bin.Count
	}
	if histogramCount != int(snapshot.Count) {
		t.Fatalf("histogram count = %d, want %d", histogramCount, snapshot.Count)
	}
}

func TestStreamReportCharts(t *testing.T) {
	report := NewStreamReport(false)

	report.lock.Lock()
	report.latencyWithinSec.Update(float64(10 * time.Millisecond))
	report.latencyWithinSec.Update(float64(20 * time.Millisecond))
	report.rpsWithinSec = 123.45
	report.codes[200] = 7
	report.concurrencyCount = 4
	report.noDateWithinSec = false
	report.lock.Unlock()

	charts := report.Charts()
	if charts == nil {
		t.Fatal("Charts() = nil, want report")
	}
	if charts.RPS != 123.45 || charts.Concurrency != 4 {
		t.Fatalf("Charts() = %+v, want RPS 123.45 and Concurrency 4", charts)
	}
	if charts.Latency.count != 2 || charts.Latency.min != float64(10*time.Millisecond) || charts.Latency.max != float64(20*time.Millisecond) {
		t.Fatalf("Latency = %+v, want two samples from 10ms to 20ms", charts.Latency)
	}
	if charts.CodeMap[200] != 7 {
		t.Fatalf("CodeMap = %#v, want 200 => 7", charts.CodeMap)
	}

	charts.CodeMap[200] = 99
	if report.codes[200] != 7 {
		t.Fatalf("Charts() returned a mutable internal CodeMap; report code count = %d, want 7", report.codes[200])
	}

	report.lock.Lock()
	report.noDateWithinSec = true
	report.lock.Unlock()
	if got := report.Charts(); got != nil {
		t.Fatalf("Charts() = %+v when noDateWithinSec is true, want nil", got)
	}
}
