package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	maxBarLen  = 40
	barStart   = "|"
	barBody    = "■"
	barEnd     = "|"
	barSpinner = []string{"|", "/", "-", "\\"}
	clearLine  = []byte("\r\033[K")
	isTerminal = isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
)

type Printer struct {
	maxNum      int64
	maxDuration time.Duration
	curNum      int64
	curDuration time.Duration
	pbInc       int64
	pbNumStr    string
	pbDurStr    string
	noClean     bool
	summary     bool
}

func NewPrinter(maxNum int64, maxDuration time.Duration, noCleanBar, summary bool) *Printer {
	return &Printer{maxNum: maxNum, maxDuration: maxDuration, noClean: noCleanBar, summary: summary}
}

func (p *Printer) updateProgressValue(rs *SnapshotReport) {
	p.pbInc++
	if p.maxDuration > 0 {
		n := rs.Elapsed
		if n > p.maxDuration {
			n = p.maxDuration
		}
		p.curDuration = n
		barLen := int((p.curDuration*time.Duration(maxBarLen-2) + p.maxDuration/2) / p.maxDuration)
		p.pbDurStr = barStart + strings.Repeat(barBody, barLen) + strings.Repeat(" ", maxBarLen-2-barLen) + barEnd
	}
	if p.maxNum > 0 {
		p.curNum = rs.Count
		if p.maxNum > 0 {
			barLen := int((p.curNum*int64(maxBarLen-2) + p.maxNum/2) / p.maxNum)
			p.pbNumStr = barStart + strings.Repeat(barBody, barLen) + strings.Repeat(" ", maxBarLen-2-barLen) + barEnd
		} else {
			idx := p.pbInc % int64(len(barSpinner))
			p.pbNumStr = barSpinner[int(idx)]
		}
	}
}

func (p *Printer) PrintLoop(snapshot func() *SnapshotReport, interval time.Duration, useSeconds bool, json bool, doneChan <-chan struct{}) {
	var buf bytes.Buffer

	var backCursor string
	cl := clearLine
	if p.summary || interval == 0 || !isTerminal {
		cl = nil
	}
	echo := func(isFinal bool) {
		report := snapshot()
		p.updateProgressValue(report)
		os.Stdout.WriteString(backCursor)
		buf.Reset()
		if json {
			p.formatJSONReports(&buf, report, isFinal, useSeconds)
		} else {
			p.formatTableReports(&buf, report, isFinal, useSeconds)
		}
		result := buf.Bytes()
		n := 0
		for {
			i := bytes.IndexByte(result, '\n')
			if i == -1 {
				os.Stdout.Write(cl)
				os.Stdout.Write(result)
				break
			}
			n++
			os.Stdout.Write(cl)
			os.Stdout.Write(result[:i])
			os.Stdout.Write([]byte("\n"))
			result = result[i+1:]
		}
		os.Stdout.Sync()
		if isTerminal {
			backCursor = fmt.Sprintf("\033[%dA", n)
		}
	}

	if interval > 0 {
		ticker := time.NewTicker(interval)
	loop:
		for {
			select {
			case <-ticker.C:
				if !p.summary {
					echo(false)
				}
			case <-doneChan:
				ticker.Stop()
				break loop
			}
		}
	} else {
		<-doneChan
	}
	echo(true)
}

//nolint
const (
	FgBlackColor int = iota + 30
	FgRedColor
	FgGreenColor
	FgYellowColor
	FgBlueColor
	FgMagentaColor
	FgCyanColor
	FgWhiteColor
)

func colorize(s string, seq int) string {
	if !isTerminal {
		return s
	}
	return fmt.Sprintf("\033[%dm%s\033[0m", seq, s)
}

func durationToString(d time.Duration, useSeconds bool) string {
	d = d.Truncate(time.Microsecond)
	if useSeconds {
		return formatFloat64(d.Seconds())
	}
	return d.String()
}

func alignBulk(bulk [][]string, aligns ...int) {
	maxLen := map[int]int{}
	for _, b := range bulk {
		for i, bb := range b {
			lbb := displayWidth(bb)
			if maxLen[i] < lbb {
				maxLen[i] = lbb
			}
		}
	}
	for _, b := range bulk {
		for i, ali := range aligns {
			if len(b) >= i+1 {
				if i == len(aligns)-1 && ali == AlignLeft {
					continue
				}
				b[i] = padString(b[i], " ", maxLen[i], ali)
			}
		}
	}
}

func writeBulkWith(writer *bytes.Buffer, bulk [][]string, lineStart, sep, lineEnd string) {
	for _, b := range bulk {
		writer.WriteString(lineStart)
		writer.WriteString(b[0])
		for _, bb := range b[1:] {
			writer.WriteString(sep)
			writer.WriteString(bb)
		}
		writer.WriteString(lineEnd)
	}
}

func writeBulk(writer *bytes.Buffer, bulk [][]string) {
	writeBulkWith(writer, bulk, "  ", "  ", "\n")
}

func formatFloat64(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func (p *Printer) formatJSONReports(writer *bytes.Buffer, snapshot *SnapshotReport, isFinal bool, useSeconds bool) {
	indent := 0
	writer.WriteString("{\n")
	indent++
	p.buildJSONSummary(writer, snapshot, indent)
	if len(snapshot.Errors) != 0 {
		writer.WriteString(",\n")
		p.buildJSONErrors(writer, snapshot, indent)
	}
	writer.WriteString(",\n")
	p.buildJSONStats(writer, snapshot, useSeconds, indent)
	writer.WriteString(",\n")
	p.buildJSONPercentile(writer, snapshot, useSeconds, indent)
	writer.WriteString(",\n")
	p.buildJSONHistogram(writer, snapshot, useSeconds, indent)
	writer.WriteString("\n}\n")
}

func (p *Printer) formatTableReports(writer *bytes.Buffer, snapshot *SnapshotReport, isFinal bool, useSeconds bool) {
	summaryBulk := p.buildSummary(snapshot, isFinal)
	errorsBulks := p.buildErrors(snapshot)
	statsBulk := p.buildStats(snapshot, useSeconds)
	percBulk := p.buildPercentile(snapshot, useSeconds)
	hisBulk := p.buildHistogram(snapshot, useSeconds, isFinal)

	writer.WriteString("Summary:\n")
	writeBulk(writer, summaryBulk)
	writer.WriteString("\n")

	if errorsBulks != nil {
		writer.WriteString("Error:\n")
		writeBulk(writer, errorsBulks)
		writer.WriteString("\n")
	}

	writeBulkWith(writer, statsBulk, "", "  ", "\n")
	writer.WriteString("\n")

	writer.WriteString("Latency Percentile:\n")
	writeBulk(writer, percBulk)
	writer.WriteString("\n")

	writer.WriteString("Latency Histogram:\n")
	writeBulk(writer, hisBulk)
}

func (p *Printer) buildJSONHistogram(writer *bytes.Buffer, snapshot *SnapshotReport, useSeconds bool, indent int) {
	tab0 := strings.Repeat("  ", indent)
	writer.WriteString(tab0 + "\"Histograms\": [\n")
	tab1 := strings.Repeat("  ", indent+1)

	maxCount := 0
	hisSum := 0
	for _, bin := range snapshot.Histograms {
		if maxCount < bin.Count {
			maxCount = bin.Count
		}
		hisSum += bin.Count
	}
	for i, bin := range snapshot.Histograms {
		writer.WriteString(fmt.Sprintf(`%s[ "%s", %d ]`, tab1,
			durationToString(bin.Mean, useSeconds), bin.Count))
		if i != len(snapshot.Histograms)-1 {
			writer.WriteString(",")
		}
		writer.WriteString("\n")
	}
	writer.WriteString(tab0 + "]")
}

func (p *Printer) buildHistogram(snapshot *SnapshotReport, useSeconds bool, isFinal bool) [][]string {
	hisBulk := make([][]string, 0, 8)
	maxCount := 0
	hisSum := 0
	for _, bin := range snapshot.Histograms {
		if maxCount < bin.Count {
			maxCount = bin.Count
		}
		hisSum += bin.Count
	}
	for _, bin := range snapshot.Histograms {
		row := []string{durationToString(bin.Mean, useSeconds), strconv.Itoa(bin.Count)}
		if isFinal {
			row = append(row, fmt.Sprintf("%.2f%%", math.Floor(float64(bin.Count)*1e4/float64(hisSum)+0.5)/100.0))
		}
		if !isFinal || p.noClean {
			barLen := 0
			if maxCount > 0 {
				barLen = (bin.Count*maxBarLen + maxCount/2) / maxCount
			}
			row = append(row, strings.Repeat(barBody, barLen))
		}
		hisBulk = append(hisBulk, row)
	}
	if isFinal {
		alignBulk(hisBulk, AlignLeft, AlignRight, AlignRight)
	} else {
		alignBulk(hisBulk, AlignLeft, AlignRight, AlignLeft)
	}
	return hisBulk
}

func (p *Printer) buildJSONPercentile(writer *bytes.Buffer, snapshot *SnapshotReport, useSeconds bool, indent int) {
	tab0 := strings.Repeat("  ", indent)
	writer.WriteString(tab0 + "\"Percentiles\": {\n")
	tab1 := strings.Repeat("  ", indent+1)
	for i, percentile := range snapshot.Percentiles {
		perc := formatFloat64(percentile.Percentile * 100)
		writer.WriteString(fmt.Sprintf(`%s"%s": "%s"`, tab1, "P"+perc,
			durationToString(percentile.Latency, useSeconds)))
		if i != len(snapshot.Percentiles)-1 {
			writer.WriteString(",")
		}
		writer.WriteString("\n")
	}
	writer.WriteString(tab0 + "}")
}

func (p *Printer) buildPercentile(snapshot *SnapshotReport, useSeconds bool) [][]string {
	percBulk := make([][]string, 2)
	percAligns := make([]int, 0, len(snapshot.Percentiles))
	for _, percentile := range snapshot.Percentiles {
		perc := formatFloat64(percentile.Percentile * 100)
		percBulk[0] = append(percBulk[0], "P"+perc)
		percBulk[1] = append(percBulk[1], durationToString(percentile.Latency, useSeconds))
		percAligns = append(percAligns, AlignCenter)
	}
	percAligns[0] = AlignLeft
	alignBulk(percBulk, percAligns...)
	return percBulk
}

func (p *Printer) buildJSONStats(writer *bytes.Buffer, snapshot *SnapshotReport, useSeconds bool, indent int) {
	tab0 := strings.Repeat("  ", indent)
	writer.WriteString(tab0 + "\"Statistics\": {\n")
	tab1 := strings.Repeat("  ", indent+1)
	writer.WriteString(fmt.Sprintf(`%s"Latency": { "Min": "%s", "Mean": "%s", "StdDev": "%s", "Max": "%s" }`,
		tab1,
		durationToString(snapshot.Stats.Min, useSeconds),
		durationToString(snapshot.Stats.Mean, useSeconds),
		durationToString(snapshot.Stats.StdDev, useSeconds),
		durationToString(snapshot.Stats.Max, useSeconds),
	))
	if snapshot.RpsStats != nil {
		writer.WriteString(",\n")
		writer.WriteString(fmt.Sprintf(`%s"RPS": { "Min": %s, "Mean": %s, "StdDev": %s, "Max": %s }`,
			tab1,
			formatFloat64(math.Trunc(snapshot.RpsStats.Min*100)/100.0),
			formatFloat64(math.Trunc(snapshot.RpsStats.Mean*100)/100.0),
			formatFloat64(math.Trunc(snapshot.RpsStats.StdDev*100)/100.0),
			formatFloat64(math.Trunc(snapshot.RpsStats.Max*100)/100.0),
		))
	}
	writer.WriteString("\n" + tab0 + "}")
}

func (p *Printer) buildStats(snapshot *SnapshotReport, useSeconds bool) [][]string {
	var statsBulk [][]string
	statsBulk = append(statsBulk,
		[]string{"Statistics", "Min", "Mean", "StdDev", "Max"},
		[]string{
			"  Latency",
			durationToString(snapshot.Stats.Min, useSeconds),
			durationToString(snapshot.Stats.Mean, useSeconds),
			durationToString(snapshot.Stats.StdDev, useSeconds),
			durationToString(snapshot.Stats.Max, useSeconds),
		},
	)
	if snapshot.RpsStats != nil {
		statsBulk = append(statsBulk,
			[]string{
				"  RPS",
				formatFloat64(math.Trunc(snapshot.RpsStats.Min*100) / 100.0),
				formatFloat64(math.Trunc(snapshot.RpsStats.Mean*100) / 100.0),
				formatFloat64(math.Trunc(snapshot.RpsStats.StdDev*100) / 100.0),
				formatFloat64(math.Trunc(snapshot.RpsStats.Max*100) / 100.0),
			},
		)
	}
	alignBulk(statsBulk, AlignLeft, AlignCenter, AlignCenter, AlignCenter, AlignCenter)
	return statsBulk
}

func (p *Printer) buildJSONErrors(writer *bytes.Buffer, snapshot *SnapshotReport, indent int) {
	tab0 := strings.Repeat("  ", indent)
	writer.WriteString(tab0 + "\"Error\": {\n")
	tab1 := strings.Repeat("  ", indent+1)
	errors := sortMapStrInt(snapshot.Errors)
	for i, v := range errors {
		v[1] = colorize(v[1], FgRedColor)
		vb, _ := json.Marshal(v[0])
		writer.WriteString(fmt.Sprintf(`%s%s: %s`, tab1, vb, v[1]))
		if i != len(errors)-1 {
			writer.WriteString(",")
		}
		writer.WriteString("\n")
	}
	writer.WriteString(tab0 + "}")
}

func (p *Printer) buildErrors(snapshot *SnapshotReport) [][]string {
	var errorsBulks [][]string
	for k, v := range snapshot.Errors {
		vs := colorize(strconv.FormatInt(v, 10), FgRedColor)
		errorsBulks = append(errorsBulks, []string{vs, "\"" + k + "\""})
	}
	if errorsBulks != nil {
		sort.Slice(errorsBulks, func(i, j int) bool { return errorsBulks[i][1] < errorsBulks[j][1] })
	}
	alignBulk(errorsBulks, AlignLeft, AlignLeft)
	return errorsBulks
}

func sortMapStrInt(m map[string]int64) (ret [][]string) {
	for k, v := range m {
		ret = append(ret, []string{k, strconv.FormatInt(v, 10)})
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i][0] < ret[j][0] })
	return
}

func (p *Printer) buildJSONSummary(writer *bytes.Buffer, snapshot *SnapshotReport, indent int) {
	tab0 := strings.Repeat("  ", indent)
	writer.WriteString(tab0 + "\"Summary\": {\n")
	{
		tab1 := strings.Repeat("  ", indent+1)
		writer.WriteString(fmt.Sprintf("%s\"Elapsed\": \"%s\",\n", tab1, snapshot.Elapsed.Truncate(100*time.Millisecond).String()))
		writer.WriteString(fmt.Sprintf("%s\"Count\": %d,\n", tab1, snapshot.Count))
		writer.WriteString(fmt.Sprintf("%s\"Counts\": {\n", tab1))
		i := 0
		tab2 := strings.Repeat("  ", indent+2)
		codes := sortMapStrInt(snapshot.Codes)
		for _, v := range codes {
			i++
			if v[0] != "2xx" {
				v[1] = colorize(v[1], FgMagentaColor)
			}
			writer.WriteString(fmt.Sprintf(`%s"%s": %s`, tab2, v[0], v[1]))
			if i != len(snapshot.Codes) {
				writer.WriteString(",")
			}
			writer.WriteString("\n")
		}
		writer.WriteString(tab1 + "},\n")
		writer.WriteString(fmt.Sprintf("%s\"RPS\": %.3f,\n", tab1, snapshot.RPS))
		writer.WriteString(fmt.Sprintf("%s\"Reads\": \"%.3fMB/s\",\n", tab1, snapshot.ReadThroughput))
		writer.WriteString(fmt.Sprintf("%s\"Writes\": \"%.3fMB/s\"\n", tab1, snapshot.WriteThroughput))
	}
	writer.WriteString(tab0 + "}")
}

func (p *Printer) buildSummary(snapshot *SnapshotReport, isFinal bool) [][]string {
	summarybulk := make([][]string, 0, 8)
	elapsedLine := []string{"Elapsed", snapshot.Elapsed.Truncate(100 * time.Millisecond).String()}
	if p.maxDuration > 0 && !isFinal {
		elapsedLine = append(elapsedLine, p.pbDurStr)
	}
	countLine := []string{"Count", strconv.FormatInt(snapshot.Count, 10)}
	if p.maxNum > 0 && !isFinal {
		countLine = append(countLine, p.pbNumStr)
	}
	summarybulk = append(
		summarybulk,
		elapsedLine,
		countLine,
	)

	codes := sortMapStrInt(snapshot.Codes)
	for _, v := range codes {
		if v[0] != "2xx" {
			v[1] = colorize(v[1], FgMagentaColor)
		}
		summarybulk = append(summarybulk, []string{"  " + v[0], v[1]})
	}
	summarybulk = append(summarybulk,
		[]string{"RPS", fmt.Sprintf("%.3f", snapshot.RPS)},
		[]string{"Reads", fmt.Sprintf("%.3fMB/s", snapshot.ReadThroughput)},
		[]string{"Writes", fmt.Sprintf("%.3fMB/s", snapshot.WriteThroughput)},
	)
	alignBulk(summarybulk, AlignLeft, AlignRight)
	return summarybulk
}

var ansi = regexp.MustCompile("\033\\[(?:[0-9]{1,3}(?:;[0-9]{1,3})*)?[m|K]")

func displayWidth(str string) int {
	return runewidth.StringWidth(ansi.ReplaceAllLiteralString(str, ""))
}

const (
	AlignLeft = iota
	AlignRight
	AlignCenter
)

func padString(s, pad string, width int, align int) string {
	gap := width - displayWidth(s)
	if gap > 0 {
		if align == AlignLeft {
			return s + strings.Repeat(pad, gap)
		} else if align == AlignRight {
			return strings.Repeat(pad, gap) + s
		} else if align == AlignCenter {
			gapLeft := gap / 2
			gapRight := gap - gapLeft
			return strings.Repeat(pad, gapLeft) + s + strings.Repeat(pad, gapRight)
		}
	}
	return s
}
