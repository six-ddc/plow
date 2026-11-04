package main

import (
	"math"
	"reflect"
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

func TestStreamReportSeries(t *testing.T) {
	oldStartTime := atomic.LoadInt64(&startTimeUnixNano)
	t.Cleanup(func() { atomic.StoreInt64(&startTimeUnixNano, oldStartTime) })
	atomic.StoreInt64(&startTimeUnixNano, time.Now().UnixNano())

	report := NewStreamReport(false)
	history := &History{}
	history.Record(report.Seconds())
	records := make(chan *ReportRecord, 3)
	done := make(chan struct{})
	go func() {
		report.Collect(records, nil)
		close(done)
	}()

	records <- &ReportRecord{cost: 10 * time.Millisecond, code: 200, concurrencyCount: 2}
	records <- &ReportRecord{cost: 30 * time.Millisecond, code: 503, concurrencyCount: 2}
	records <- &ReportRecord{cost: 20 * time.Millisecond, error: "timeout", concurrencyCount: 2}
	time.Sleep(1100 * time.Millisecond)
	close(records)
	<-done

	<-history.done
	series := history.series
	if len(series) != 1 {
		t.Fatalf("series = %+v, want one point", series)
	}
	p := series[0]
	if p.Count != 3 || p.Codes[200] != 1 || p.Codes[503] != 1 || len(p.Codes) != 2 || p.Concurrency != 2 || p.RPS <= 0 || p.Phases != nil {
		t.Fatalf("point = %+v, want 3 requests, codes 200 and 503, concurrency 2 and positive RPS", p)
	}
	if p.Min != 10*time.Millisecond || p.Mean != 20*time.Millisecond || p.Max != 30*time.Millisecond {
		t.Fatalf("point latency = %+v, want 10ms / 20ms / 30ms", p)
	}
	if p.Elapsed < time.Second || p.Elapsed > 2*time.Second {
		t.Fatalf("point elapsed = %s, want about 1s", p.Elapsed)
	}
}

func TestMergeSeries(t *testing.T) {
	points := []SeriesPoint{
		{Elapsed: 1 * time.Second, Count: 10, RPS: 10, Min: 2 * time.Millisecond, Mean: 4 * time.Millisecond, Max: 6 * time.Millisecond, Codes: map[int]int64{200: 10}, Concurrency: 1, Phases: []time.Duration{2, 0}},
		{Elapsed: 2 * time.Second, Count: 30, RPS: 30, Min: 1 * time.Millisecond, Mean: 8 * time.Millisecond, Max: 9 * time.Millisecond, Codes: map[int]int64{200: 40}, Concurrency: 2, Phases: []time.Duration{4, 0}},
		{Elapsed: 3 * time.Second, Concurrency: 2},
		{Elapsed: 4 * time.Second, Count: 20, RPS: 20, Min: 3 * time.Millisecond, Mean: 3 * time.Millisecond, Max: 3 * time.Millisecond, Concurrency: 3},
		{Elapsed: 5 * time.Second, Count: 5, RPS: 5, Min: 5 * time.Millisecond, Mean: 5 * time.Millisecond, Max: 5 * time.Millisecond, Concurrency: 3},
	}

	if same := mergeSeries(points, 5); len(same) != 5 {
		t.Fatalf("mergeSeries within the limit = %+v, want the points", same)
	}

	got := mergeSeries(points, 3)
	want := []SeriesPoint{
		{Elapsed: 2 * time.Second, Count: 40, RPS: 20, Min: 1 * time.Millisecond, Mean: 7 * time.Millisecond, Max: 9 * time.Millisecond, Codes: map[int]int64{200: 40}, Concurrency: 2, Phases: []time.Duration{3, 0}},
		{Elapsed: 4 * time.Second, Count: 20, RPS: 10, Min: 3 * time.Millisecond, Mean: 3 * time.Millisecond, Max: 3 * time.Millisecond, Concurrency: 3},
		{Elapsed: 5 * time.Second, Count: 5, RPS: 5, Min: 5 * time.Millisecond, Mean: 5 * time.Millisecond, Max: 5 * time.Millisecond, Concurrency: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeSeries = %+v, want %+v", got, want)
	}
}

func TestStreamReportSecondsClosedWithoutAnySecond(t *testing.T) {
	report := NewStreamReport(false)
	history := &History{}
	history.Record(report.Seconds())
	records := make(chan *ReportRecord, 1)
	go report.Collect(records, nil)
	records <- &ReportRecord{cost: time.Millisecond, code: 200}
	close(records)
	<-report.Done()

	select {
	case <-history.done:
		if len(history.series) != 0 {
			t.Fatalf("series = %+v, want none for a run shorter than a second", history.series)
		}
	case <-time.After(time.Second):
		t.Fatal("the recording isn't done after the collecting is done")
	}
}
