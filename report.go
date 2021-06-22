package main

import (
	"github.com/beorn7/perks/histogram"
	"github.com/beorn7/perks/quantile"
	"math"
	"sync"
	"time"
)

var quantiles = []float64{0.50, 0.75, 0.90, 0.95, 0.99, 0.999, 0.9999}

var quantilesTarget = map[float64]float64{
	0.50:   0.01,
	0.75:   0.01,
	0.90:   0.001,
	0.95:   0.001,
	0.99:   0.001,
	0.999:  0.0001,
	0.9999: 0.00001,
}

type Stats struct {
	count int64
	sum   float64
	sumSq float64
	min   float64
	max   float64
}

func (s *Stats) Update(v float64) {
	s.count++
	s.sum += v
	s.sumSq += v * v
	if v < s.min || s.count == 1 {
		s.min = v
	}
	if v > s.max || s.count == 1 {
		s.max = v
	}
}

func (s *Stats) Stddev() float64 {
	num := (float64(s.count) * s.sumSq) - math.Pow(s.sum, 2)
	div := float64(s.count * (s.count - 1))
	if div == 0 {
		return 0
	}
	return math.Sqrt(num / div)
}

func (s *Stats) Mean() float64 {
	if s.count == 0 {
		return 0
	}
	return s.sum / float64(s.count)
}

func (s *Stats) Reset() {
	s.count = 0
	s.sum = 0
	s.sumSq = 0
	s.min = 0
	s.max = 0
}

type StreamReport struct {
	lock sync.Mutex

	latencyStats     *Stats
	rpsStats         *Stats
	latencyQuantile  *quantile.Stream
	latencyHistogram *histogram.Histogram
	codes            map[string]int64
	errors           map[string]int64

	latencyWithinSec *Stats
	rpsWithinSec     float64
	noDateWithinSec  bool

	readBytes  int64
	writeBytes int64

	doneChan chan struct{}
}

func NewStreamReport() *StreamReport {
	return &StreamReport{
		latencyQuantile:  quantile.NewTargeted(quantilesTarget),
		latencyHistogram: histogram.New(8),
		codes:            make(map[string]int64, 1),
		errors:           make(map[string]int64, 1),
		doneChan:         make(chan struct{}, 1),
		latencyStats:     &Stats{},
		rpsStats:         &Stats{},
		latencyWithinSec: &Stats{},
	}
}

func (s *StreamReport) insert(v float64) {
	s.latencyQuantile.Insert(v)
	s.latencyHistogram.Insert(v)

	s.latencyStats.Update(v)
}

func (s *StreamReport) Collect(records <-chan *ReportRecord) {
	latencyWithinSecTemp := &Stats{}
	go func() {
		ticker := time.NewTicker(time.Second)
		lastCount := int64(0)
		lastTime := startTime
		for {
			select {
			case <-ticker.C:
				s.lock.Lock()
				dc := s.latencyStats.count - lastCount
				if dc > 0 {
					rps := float64(dc) / time.Since(lastTime).Seconds()
					s.rpsStats.Update(rps)
					lastCount = s.latencyStats.count
					lastTime = time.Now()

					*s.latencyWithinSec = *latencyWithinSecTemp
					s.rpsWithinSec = rps
					latencyWithinSecTemp.Reset()
					s.noDateWithinSec = false
				} else {
					s.noDateWithinSec = true
				}
				s.lock.Unlock()
			case <-s.doneChan:
				return
			}
		}
	}()

	for {
		r, ok := <-records
		if !ok {
			close(s.doneChan)
			break
		}
		s.lock.Lock()
		latencyWithinSecTemp.Update(float64(r.cost))
		s.insert(float64(r.cost))
		if r.code != "" {
			s.codes[r.code] ++
		}
		if r.error != "" {
			s.errors[r.error] ++
		}
		s.readBytes = r.readBytes
		s.writeBytes = r.writeBytes
		s.lock.Unlock()
		recordPool.Put(r)
	}
}

type SnapshotReport struct {
	Elapsed         time.Duration
	Count           int64
	Codes           map[string]int64
	Errors          map[string]int64
	RPS             float64
	ReadThroughput  float64
	WriteThroughput float64

	Stats *struct {
		Min    time.Duration
		Mean   time.Duration
		StdDev time.Duration
		Max    time.Duration
	}

	RpsStats *struct {
		Min    float64
		Mean   float64
		StdDev float64
		Max    float64
	}

	Percentiles []*struct {
		Percentile float64
		Latency    time.Duration
	}

	Histograms []*struct {
		Mean  time.Duration
		Count int
	}
}

func (s *StreamReport) Snapshot() *SnapshotReport {
	s.lock.Lock()

	rs := &SnapshotReport{
		Elapsed: time.Since(startTime),
		Count:   s.latencyStats.count,
		Stats: &struct {
			Min    time.Duration
			Mean   time.Duration
			StdDev time.Duration
			Max    time.Duration
		}{time.Duration(s.latencyStats.min), time.Duration(s.latencyStats.Mean()),
			time.Duration(s.latencyStats.Stddev()), time.Duration(s.latencyStats.max)},
	}
	if s.rpsStats.count > 0 {
		rs.RpsStats = &struct {
			Min    float64
			Mean   float64
			StdDev float64
			Max    float64
		}{s.rpsStats.min, s.rpsStats.Mean(),
			s.rpsStats.Stddev(), s.rpsStats.max}
	}

	elapseInSec := rs.Elapsed.Seconds()
	rs.RPS = float64(rs.Count) / elapseInSec
	rs.ReadThroughput = float64(s.readBytes) / 1024.0 / 1024.0 / elapseInSec
	rs.WriteThroughput = float64(s.writeBytes) / 1024.0 / 1024.0 / elapseInSec

	rs.Codes = make(map[string]int64, len(s.codes))
	for k, v := range s.codes {
		rs.Codes[k] = v
	}
	rs.Errors = make(map[string]int64, len(s.errors))
	for k, v := range s.errors {
		rs.Errors[k] = v
	}

	rs.Percentiles = make([]*struct {
		Percentile float64
		Latency    time.Duration
	}, len(quantiles))
	for i, p := range quantiles {
		rs.Percentiles[i] = &struct {
			Percentile float64
			Latency    time.Duration
		}{p, time.Duration(s.latencyQuantile.Query(p))}
	}

	hisBins := s.latencyHistogram.Bins()
	rs.Histograms = make([]*struct {
		Mean  time.Duration
		Count int
	}, len(hisBins))
	for i, b := range hisBins {
		rs.Histograms[i] = &struct {
			Mean  time.Duration
			Count int
		}{time.Duration(b.Mean()), b.Count}
	}

	s.lock.Unlock()
	return rs
}

func (s *StreamReport) Done() <-chan struct{} {
	return s.doneChan
}

type ChartsReport struct {
	RPS     float64
	Latency Stats
}

func (s *StreamReport) Charts() *ChartsReport {
	s.lock.Lock()
	var cr *ChartsReport
	if s.noDateWithinSec {
		cr = nil
	} else {
		cr = &ChartsReport{
			RPS:     s.rpsWithinSec,
			Latency: *s.latencyWithinSec,
		}
	}
	s.lock.Unlock()
	return cr
}
