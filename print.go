package main

import (
	"bytes"
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
	isTerminal = isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsTerminal(os.Stdout.Fd())
)

type Printer struct {
	maxNum      int64
	maxDuration time.Duration
	curNum      int64
	curDuration time.Duration
	pbInc       int64
	pbNumStr    string
	pbDurStr    string
}

func NewPrinter(maxNum int64, maxDuration time.Duration) *Printer {
	return &Printer{maxNum: maxNum, maxDuration: maxDuration}
}

func (p *Printer) updateProgressValue(rs *SnapshotReport) {
	p.pbInc += 1
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

func (p *Printer) PrintLoop(snapshot func() *SnapshotReport, interval time.Duration, useSeconds bool, doneChan <-chan struct{}) {
	var buf bytes.Buffer

	var backCursor string
	echo := func(isFinal bool) {
		report := snapshot()
		p.updateProgressValue(report)
		os.Stdout.WriteString(backCursor)
		buf.Reset()
		p.formatTableReports(&buf, report, isFinal, useSeconds)
		result := buf.Bytes()
		n := 0
		for {
			i := bytes.IndexByte(result, '\n')
			if i == -1 {
				os.Stdout.Write(clearLine)
				os.Stdout.Write(result)
				break
			}
			n++
			os.Stdout.Write(clearLine)
			os.Stdout.Write(result[:i])
			os.Stdout.Write([]byte("\n"))
			result = result[i+1:]
		}
		os.Stdout.Sync()
		backCursor = fmt.Sprintf("\033[%dA", n)
	}

	if interval > 0 {
		ticker := time.NewTicker(interval)
	loop:
		for {
			select {
			case <-ticker.C:
				echo(false)
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
				if i == len(aligns)-1 && ali == ALIGN_LEFT {
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
		} else {
			barLen := 0
			if maxCount > 0 {
				barLen = (bin.Count*maxBarLen + maxCount/2) / maxCount
			}
			row = append(row, strings.Repeat(barBody, barLen))
		}
		hisBulk = append(hisBulk, row)
	}
	if isFinal {
		alignBulk(hisBulk, ALIGN_LEFT, ALIGN_RIGHT, ALIGN_RIGHT)
	} else {
		alignBulk(hisBulk, ALIGN_LEFT, ALIGN_RIGHT, ALIGN_LEFT)
	}
	return hisBulk
}

func (p *Printer) buildPercentile(snapshot *SnapshotReport, useSeconds bool) [][]string {
	percBulk := make([][]string, 2)
	percAligns := make([]int, 0, len(snapshot.Percentiles))
	for _, percentile := range snapshot.Percentiles {
		perc := formatFloat64(percentile.Percentile * 100)
		percBulk[0] = append(percBulk[0], "P"+perc)
		percBulk[1] = append(percBulk[1], durationToString(percentile.Latency, useSeconds))
		percAligns = append(percAligns, ALIGN_CENTER)
	}
	percAligns[0] = ALIGN_LEFT
	alignBulk(percBulk, percAligns...)
	return percBulk
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
	alignBulk(statsBulk, ALIGN_LEFT, ALIGN_CENTER, ALIGN_CENTER, ALIGN_CENTER, ALIGN_CENTER)
	return statsBulk
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
	alignBulk(errorsBulks, ALIGN_LEFT, ALIGN_LEFT)
	return errorsBulks
}

func (p *Printer) buildSummary(snapshot *SnapshotReport, isFinal bool) [][]string {
	summarybulk := make([][]string, 0, 8)
	elapsedLine := []string{"Elapsed", snapshot.Elapsed.Truncate(time.Millisecond).String()}
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

	codesBulks := make([][]string, 0, len(snapshot.Codes))
	for k, v := range snapshot.Codes {
		vs := strconv.FormatInt(v, 10)
		if k != "2xx" {
			vs = colorize(vs, FgMagentaColor)
		}
		codesBulks = append(codesBulks, []string{"  " + k, vs})
	}
	sort.Slice(codesBulks, func(i, j int) bool { return codesBulks[i][0] < codesBulks[j][0] })
	summarybulk = append(summarybulk, codesBulks...)
	summarybulk = append(summarybulk,
		[]string{"RPS", fmt.Sprintf("%.3f", snapshot.RPS)},
		[]string{"Reads", fmt.Sprintf("%.3fMB/s", snapshot.ReadThroughput)},
		[]string{"Writes", fmt.Sprintf("%.3fMB/s", snapshot.WriteThroughput)},
	)
	alignBulk(summarybulk, ALIGN_LEFT, ALIGN_RIGHT)
	return summarybulk
}

var ansi = regexp.MustCompile("\033\\[(?:[0-9]{1,3}(?:;[0-9]{1,3})*)?[m|K]")

func displayWidth(str string) int {
	return runewidth.StringWidth(ansi.ReplaceAllLiteralString(str, ""))
}

const (
	ALIGN_LEFT = iota
	ALIGN_RIGHT
	ALIGN_CENTER
)

func padString(s, pad string, width int, align int) string {
	gap := width - displayWidth(s)
	if gap > 0 {
		if align == ALIGN_LEFT {
			return s + strings.Repeat(pad, gap)
		} else if align == ALIGN_RIGHT {
			return strings.Repeat(pad, gap) + s
		} else if align == ALIGN_CENTER {
			gapLeft := int(math.Ceil(float64(gap / 2)))
			gapRight := gap - gapLeft
			return strings.Repeat(pad, gapLeft) + s + strings.Repeat(pad, gapRight)
		}
	}
	return s
}
