package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"gopkg.in/alecthomas/kingpin.v3-unstable"
)

var (
	concurrency = kingpin.Flag("concurrency", "Number of connections to run concurrently").Short('c').Default("1").Int()
	requests    = kingpin.Flag("requests", "Number of requests to run").Short('n').Default("-1").Int64()
	duration    = kingpin.Flag("duration", "Duration of test, examples: -d 10s -d 3m").Short('d').PlaceHolder("DURATION").Duration()
	interval    = kingpin.Flag("interval", "Print snapshot result every interval, use 0 to print once at the end").Short('i').Default("200ms").Duration()
	seconds     = kingpin.Flag("seconds", "Use seconds as time unit to print").Bool()

	body        = kingpin.Flag("body", "HTTP request body, if start the body with @, the rest should be a filename to read").String()
	stream      = kingpin.Flag("stream", "Specify whether to stream file specified by '--body @file' using chunked encoding or to read into memory").Default("false").Bool()
	method      = kingpin.Flag("method", "HTTP method").Default("GET").Short('m').String()
	headers     = kingpin.Flag("header", "Custom HTTP headers").Short('H').PlaceHolder("K:V").Strings()
	host        = kingpin.Flag("host", "Host header").String()
	contentType = kingpin.Flag("content", "Content-Type header").Short('T').String()

	chartsListenAddr = kingpin.Flag("listen", "Listen addr to serve Web UI").Default(":18888").String()
	timeout          = kingpin.Flag("timeout", "Timeout for each http request").PlaceHolder("DURATION").Duration()
	dialTimeout      = kingpin.Flag("dial-timeout", "Timeout for dial addr").PlaceHolder("DURATION").Duration()
	reqWriteTimeout  = kingpin.Flag("req-timeout", "Timeout for full request writing").PlaceHolder("DURATION").Duration()
	respReadTimeout  = kingpin.Flag("resp-timeout", "Timeout for full response reading").PlaceHolder("DURATION").Duration()
	socks5           = kingpin.Flag("socks5", "Socks5 proxy").PlaceHolder("ip:port").String()

	url = kingpin.Arg("url", "request url").Required().String()
)

func errAndExit(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func main() {
	kingpin.UsageTemplate(kingpin.CompactUsageTemplate).Version("1.0.0").Author("six-ddc@github")
	kingpin.CommandLine.Help = `A high-performance HTTP benchmarking tool with real-time web UI and terminal displaying

Example:
  plow http://127.0.0.1:8080/ -c 20 -n 100000
  plow https://httpbin.org/post -c 20 -d 5m --body @file.json -T 'application/json' -m POST
`
	kingpin.Parse()
	if *requests >= 0 && *requests < int64(*concurrency) {
		errAndExit("requests must greater than or equal concurrency")
		return
	}

	var err error
	var bodyBytes []byte
	var bodyFile string
	if strings.HasPrefix(*body, "@") {
		fileName := (*body)[1:]
		if _, err = os.Stat(fileName); err != nil {
			errAndExit(err.Error())
			return
		}
		if *stream {
			bodyFile = fileName
		} else {
			bodyBytes, err = ioutil.ReadFile(fileName)
			if err != nil {
				errAndExit(err.Error())
				return
			}
		}
	} else if *body != "" {
		bodyBytes = []byte(*body)
	}

	clientOpt := ClientOpt{
		url:          *url,
		method:       *method,
		headers:      *headers,
		bodyBytes:    bodyBytes,
		bodyFile:     bodyFile,
		maxConns:     *concurrency,
		doTimeout:    *timeout,
		readTimeout:  *respReadTimeout,
		writeTimeout: *reqWriteTimeout,
		dialTimeout:  *dialTimeout,

		socks5Proxy: *socks5,
		contentType: *contentType,
		host:        *host,
	}

	var desc string
	desc = fmt.Sprintf("Benchmarking %s", *url)
	if *requests > 0 {
		desc += fmt.Sprintf(" with %d request(s)", *requests)
	}
	if *duration > 0 {
		desc += fmt.Sprintf(" for %s", duration.String())
	}
	desc += fmt.Sprintf(" using %d connection(s).", *concurrency)
	fmt.Println(desc)

	requester, err := NewRequester(*concurrency, *requests, *duration, &clientOpt)
	if err != nil {
		errAndExit(err.Error())
		return
	}
	go requester.Run()

	report := NewStreamReport()
	go report.Collect(requester.RecordChan())

	if *chartsListenAddr != "" {
		charts, err := NewCharts(*chartsListenAddr, report.Charts, desc)
		if err != nil {
			errAndExit(err.Error())
			return
		}
		go charts.Serve()
	}

	printer := NewPrinter(*requests, *duration)
	printer.PrintLoop(report.Snapshot, *interval, *seconds, report.Done())
}
