package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	cors "github.com/AdhityaRamadhanus/fasthttpcors"
	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/go-echarts/v2/templates"
	"github.com/valyala/fasthttp"
	"net"
	"strings"
	"sync"
	"text/template"
	"time"
)

func init() {
	templates.PageTpl = `
{{- define "page" }}
<!DOCTYPE html>
<html>
    {{- template "header" . }}
<body>
<p align="center">🚀 <a href="https://github.com/six-ddc/plow"><b>plow</b></a> <em>is a high-performance HTTP benchmarking tool with real-time web UI and terminal displaying</em></p>
<style> .box { justify-content:center; display:flex; flex-wrap:wrap } </style>
<div class="box"> {{- range .Charts }} {{ template "base" . }} {{- end }} </div>
</body>
</html>
{{ end }}
`
}

var (
	assertsPath     = "/echarts/statics/"
	apiPath         = "/data"
	latencyView     = "latency"
	rpsView         = "rps"
	timeFormat      = "15:04:05"
	refreshInterval = time.Second
)

const (
	DefaultTemplate string = `
$(function () { setInterval({{ .ViewID }}_sync, {{ .Interval }}); });
function {{ .ViewID }}_sync() {
    $.ajax({
        type: "GET",
        url: "http://{{ .Addr }}{{ .ApiPath }}/{{ .Route }}",
        dataType: "json",
        success: function (result) {
            let opt = goecharts_{{ .ViewID }}.getOption();
            let x = opt.xAxis[0].data;
            x.push(result.time);
            opt.xAxis[0].data = x;
            for (let i = 0; i < result.values.length; i++) {
                let y = opt.series[i].data;
                y.push({ value: result.values[i] });
                opt.series[i].data = y;
                goecharts_{{ .ViewID }}.setOption(opt);
            }
        }
    });
}`
)

func (c *Charts) genViewTemplate(vid, route string) string {
	tpl, err := template.New("view").Parse(DefaultTemplate)
	if err != nil {
		panic("failed to parse template " + err.Error())
	}

	var d = struct {
		Interval int
		Addr     string
		ApiPath  string
		Route    string
		ViewID   string
	}{
		Interval: int(refreshInterval.Milliseconds()),
		Addr:     c.linkAddr,
		ApiPath:  apiPath,
		Route:    route,
		ViewID:   vid,
	}

	buf := bytes.Buffer{}
	if err := tpl.Execute(&buf, d); err != nil {
		panic("statsview: failed to execute template " + err.Error())
	}

	return buf.String()
}

func (c *Charts) newBasicView(route string) *charts.Line {
	graph := charts.NewLine()
	graph.SetGlobalOptions(
		charts.WithTooltipOpts(opts.Tooltip{Show: true, Trigger: "axis"}),
		charts.WithXAxisOpts(opts.XAxis{Name: "Time"}),
		charts.WithInitializationOpts(opts.Initialization{
			Width:  "700px",
			Height: "400px",
		}),
		charts.WithDataZoomOpts(opts.DataZoom{
			Type:       "slider",
			XAxisIndex: []int{0},
		}),
	)
	graph.SetXAxis([]string{}).SetSeriesOptions(charts.WithLineChartOpts(opts.LineChart{Smooth: true}))
	graph.AddJSFuncs(c.genViewTemplate(graph.ChartID, route))
	return graph
}

func (c *Charts) newLatencyView() components.Charter {
	graph := c.newBasicView(latencyView)
	graph.SetGlobalOptions(
		charts.WithTitleOpts(opts.Title{Title: "Latency"}),
		charts.WithYAxisOpts(opts.YAxis{Scale: true, AxisLabel: &opts.AxisLabel{Formatter: "{value} ms"}}),
		charts.WithLegendOpts(opts.Legend{Show: true, Selected: map[string]bool{"Min": false, "Max": false}}),
	)
	graph.AddSeries("Min", []opts.LineData{}).
		AddSeries("Mean", []opts.LineData{}).
		AddSeries("Max", []opts.LineData{})
	return graph
}

func (c *Charts) newRPSView() components.Charter {
	graph := c.newBasicView(rpsView)
	graph.SetGlobalOptions(
		charts.WithTitleOpts(opts.Title{Title: "Reqs/sec"}),
		charts.WithYAxisOpts(opts.YAxis{Scale: true}),
	)
	graph.AddSeries("RPS", []opts.LineData{})
	return graph
}

type Metrics struct {
	Values []float64 `json:"values"`
	Time   string    `json:"time"`
}

type Charts struct {
	listenAddr string
	linkAddr   string
	page       *components.Page
	ln         net.Listener
	lock       sync.Mutex
	reportData ChartsReport
	dataFunc   func() *ChartsReport
}

func NewCharts(listenAddr string, linkAddr string, dataFunc func() *ChartsReport) (*Charts, error) {
	ln, err := net.Listen("tcp4", listenAddr)
	if err != nil {
		return nil, err
	}
	c := &Charts{listenAddr: listenAddr, linkAddr: linkAddr, ln: ln, dataFunc: dataFunc}

	c.page = components.NewPage()
	c.page.PageTitle = "plow"
	c.page.AssetsHost = fmt.Sprintf("http://%s%s", linkAddr, assertsPath)
	c.page.Assets.JSAssets.Add("jquery.min.js")
	c.page.AddCharts(c.newLatencyView(), c.newRPSView())

	return c, nil
}

func (c *Charts) Handler(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	switch path {
	case assertsPath + "echarts.min.js":
		ctx.WriteString(EchartJS)
	case assertsPath + "jquery.min.js":
		ctx.WriteString(JqueryJS)
	case "/":
		ctx.SetContentType("text/html")
		c.page.Render(ctx)
	default:
		if strings.HasPrefix(path, apiPath) {
			view := path[len(apiPath)+1:]
			var values []float64
			c.lock.Lock()
			switch view {
			case latencyView:
				values = append(values, c.dataFunc().Latency.min/1e6)
				values = append(values, c.dataFunc().Latency.Mean()/1e6)
				values = append(values, c.dataFunc().Latency.max/1e6)
			case rpsView:
				values = append(values, c.dataFunc().RPS)
			}
			c.lock.Unlock()
			metrics := &Metrics{
				Time:   time.Now().Format(timeFormat),
				Values: values,
			}
			json.NewEncoder(ctx).Encode(metrics)
		} else {
			ctx.Error("NotFound", fasthttp.StatusNotFound)
		}
	}
}

func (c *Charts) Serve() {
	go func() {
		ticker := time.NewTicker(refreshInterval)
		for {
			select {
			case <-ticker.C:
				c.lock.Lock()
				c.reportData = *c.dataFunc()
				c.lock.Unlock()
			}
		}
	}()
	server := fasthttp.Server{
		Handler: cors.DefaultHandler().CorsMiddleware(c.Handler),
	}
	server.Serve(c.ln)
}
