package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const historyVersion = 1

// the IDs saved before had only 4 hex digits
var historyIDPattern = regexp.MustCompile(`^\d{8}-\d{6}-([0-9a-f]{4}|[0-9a-f]{16})$`)

type HistoryRun struct {
	Version     int
	ID          string
	Time        time.Time
	PlowVersion string
	Interrupted bool
	Label       string `json:",omitempty"`
	Options     HistoryOptions
	Result      HistoryResult
	// the runs saved before don't have it
	Series []SeriesPoint `json:",omitempty"`
}

type HistoryOptions struct {
	URL         string
	Method      string
	Concurrency int
	Requests    int64
	Duration    time.Duration
	Rate        string
	Trace       bool
	RampUp      int
	// the hash of the request settings other than the above, like body and headers,
	// to tell whether two runs are comparable without storing the secrets in them
	Fingerprint string `json:",omitempty"`
}

func requestFingerprint(opt *ClientOpt) string {
	headers := append([]string(nil), opt.headers...)
	sort.Strings(headers)
	body := sha256.Sum256(opt.bodyBytes)
	data, _ := json.Marshal([]interface{}{
		hex.EncodeToString(body[:]), opt.bodyFile, headers, opt.host, opt.contentType,
		opt.doTimeout, opt.readTimeout, opt.writeTimeout, opt.dialTimeout,
		opt.socks5Proxy, opt.httpProxy, opt.unixSocket, opt.insecure, opt.certPath,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

type HistoryResult struct {
	Elapsed         time.Duration
	Count           int64
	RPS             float64
	ReadThroughput  float64
	WriteThroughput float64
	Codes           map[string]int64
	Errors          map[string]int64
	Latency         HistoryLatency
	Percentiles     []HistoryPercentile
	Phases          []HistoryPhase `json:",omitempty"`
}

type HistoryLatency struct {
	Min    time.Duration
	Mean   time.Duration
	StdDev time.Duration
	Max    time.Duration
}

type HistoryPercentile struct {
	Percentile float64
	Latency    time.Duration
}

type HistoryPhase struct {
	Name        string
	Count       int64
	Min         time.Duration
	Mean        time.Duration
	Max         time.Duration
	Percentiles []HistoryPercentile
}

func newHistoryResult(report *SnapshotReport) HistoryResult {
	res := HistoryResult{
		Elapsed:         report.Elapsed,
		Count:           report.Count,
		RPS:             report.RPS,
		ReadThroughput:  report.ReadThroughput,
		WriteThroughput: report.WriteThroughput,
		Codes:           report.Codes,
		Errors:          report.Errors,
		Latency: HistoryLatency{
			Min:    report.Stats.Min,
			Mean:   report.Stats.Mean,
			StdDev: report.Stats.StdDev,
			Max:    report.Stats.Max,
		},
	}
	for _, p := range report.Percentiles {
		res.Percentiles = append(res.Percentiles, HistoryPercentile{p.Percentile, p.Latency})
	}
	for _, ps := range report.Phases {
		phase := HistoryPhase{Name: ps.Name, Count: ps.Count, Min: ps.Min, Mean: ps.Mean, Max: ps.Max}
		for i, q := range phaseQuantiles {
			phase.Percentiles = append(phase.Percentiles, HistoryPercentile{q, ps.Percentiles[i]})
		}
		res.Phases = append(res.Phases, phase)
	}
	return res
}

const maxSeriesPoints = 3600

// mergeSeries merges the adjacent points to keep at most limit points.
func mergeSeries(points []SeriesPoint, limit int) []SeriesPoint {
	if len(points) <= limit {
		return points
	}
	k := (len(points) + limit - 1) / limit
	res := make([]SeriesPoint, 0, (len(points)+k-1)/k)
	for i := 0; i < len(points); i += k {
		var from time.Duration
		if i > 0 {
			from = points[i-1].Elapsed
		}
		res = append(res, mergePoints(from, points[i:min(i+k, len(points))]))
	}
	return res
}

// mergePoints merges the consecutive points into one, from is the elapsed time before the first point.
func mergePoints(from time.Duration, points []SeriesPoint) SeriesPoint {
	last := points[len(points)-1]
	m := SeriesPoint{Elapsed: last.Elapsed, Codes: last.Codes, Concurrency: last.Concurrency}
	if last.Phases != nil {
		m.Phases = make([]time.Duration, len(last.Phases))
		for i := range m.Phases {
			var sum time.Duration
			var n time.Duration
			for _, p := range points {
				if i < len(p.Phases) && p.Phases[i] > 0 {
					sum += p.Phases[i]
					n++
				}
			}
			if n > 0 {
				m.Phases[i] = sum / n
			}
		}
	}
	var latencySum float64
	for _, p := range points {
		if p.Count > 0 {
			if m.Count == 0 || p.Min < m.Min {
				m.Min = p.Min
			}
			m.Max = max(m.Max, p.Max)
			latencySum += float64(p.Mean) * float64(p.Count)
		}
		m.Count += p.Count
	}
	if m.Count > 0 {
		m.Mean = time.Duration(latencySum / float64(m.Count))
		if span := last.Elapsed - from; span > 0 {
			m.RPS = float64(m.Count) / span.Seconds()
		}
	}
	return m
}

// History records the stats of each second of the run, and saves the run into its own file when it's finished.
type History struct {
	dir    string
	series []SeriesPoint
	done   chan struct{}
}

func NewHistory(dir string) (*History, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".plow", "history")
	}
	return &History{dir: dir}, nil
}

// Record keeps the stats of each second from StreamReport.Seconds until the channel is closed.
func (h *History) Record(seconds <-chan SeriesPoint) {
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		for p := range seconds {
			h.series = append(h.series, p)
		}
	}()
}

// Save waits until the recording is done, and saves the run with the final report and the stats of each second.
func (h *History) Save(run *HistoryRun, report *SnapshotReport) (string, error) {
	if h.done != nil {
		<-h.done
		run.Series = mergeSeries(h.series, maxSeriesPoints)
	}
	run.Version = historyVersion
	run.PlowVersion = version
	if run.Time.IsZero() {
		run.Time = time.Now()
	}
	run.Result = newHistoryResult(report)

	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return "", err
	}
	for {
		path, err := h.write(run)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path, nil
	}
}

// write publishes the run with a new ID by hard linking, which fails with os.ErrExist instead of
// replacing the run of another process when the ID is taken.
func (h *History) write(run *HistoryRun) (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	run.ID = run.Time.Format("20060102-150405") + "-" + hex.EncodeToString(suffix)
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(h.dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(h.dir, run.ID+".json")
	return path, os.Link(tmp.Name(), path)
}

func (h *History) Delete(id string) error {
	if !historyIDPattern.MatchString(id) {
		return fmt.Errorf("invalid history id %q", id)
	}
	return os.Remove(filepath.Join(h.dir, id+".json"))
}

// List returns the runs sorted from the newest, the files which can't be parsed are skipped.
func (h *History) List() ([]*HistoryRun, error) {
	entries, err := os.ReadDir(h.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []*HistoryRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	runs := make([]*HistoryRun, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(h.dir, e.Name()))
		if err != nil {
			continue
		}
		var run HistoryRun
		if json.Unmarshal(data, &run) != nil || run.Version != historyVersion {
			continue
		}
		run.ID = strings.TrimSuffix(e.Name(), ".json")
		runs = append(runs, &run)
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].Time.After(runs[j].Time)
	})
	return runs, nil
}
