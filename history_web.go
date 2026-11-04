package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/valyala/fasthttp"
	"gopkg.in/alecthomas/kingpin.v3-unstable"
)

var historyAPIPath = "/history/api/runs"

// startHistoryServer starts the web server to browse the runs in the history, for "plow history".
func startHistoryServer(args []string) {
	app := kingpin.New("plow history", "Browse the previous runs in the web UI").
		Resolver(kingpin.PrefixedEnvarResolver("PLOW_", ";"))
	listen := app.Flag("listen", "Listen addr to serve Web UI, use :18889 to allow the access from other hosts").Default("127.0.0.1:18889").String()
	dir := app.Flag("history-dir", "Directory of the history, default is ~/.plow/history").PlaceHolder("DIR").String()
	open := app.Flag("auto-open-browser", "Specify whether auto open browser to show the history").Bool()
	_, err := app.Parse(args)
	app.FatalIfError(err, "")

	history, err := NewHistory(*dir)
	if err != nil {
		errAndExit(err.Error())
		return
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		errAndExit(err.Error())
		return
	}
	addr := "http://" + ln.Addr().String()
	fmt.Fprintf(os.Stderr, "@ History is listening on %s, press Ctrl-C to quit\n", addr)
	if *open {
		go openBrowser(addr)
	}
	server := fasthttp.Server{Handler: HistoryHandler(history)}
	if err := server.Serve(ln); err != nil {
		errAndExit(err.Error())
	}
}

// HistoryHandler serves the page to browse all the runs in the history.
func HistoryHandler(history *History) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		if serveHistoryAPI(ctx, history) || serveAsset(ctx) {
			return
		}
		if string(ctx.Path()) == "/" {
			ctx.SetContentType("text/html; charset=utf-8")
			ctx.SetBodyString(historyPage)
			return
		}
		ctx.Error("NotFound", fasthttp.StatusNotFound)
	}
}

// serveHistoryAPI serves the API to list and delete the runs, returns false for the other paths.
func serveHistoryAPI(ctx *fasthttp.RequestCtx, history *History) bool {
	path := string(ctx.Path())
	if strings.HasPrefix(path, historyAPIPath+"/") && ctx.IsDelete() {
		err := history.Delete(path[len(historyAPIPath)+1:])
		if errors.Is(err, os.ErrNotExist) {
			ctx.Error("NotFound", fasthttp.StatusNotFound)
		} else if err != nil {
			ctx.Error(err.Error(), fasthttp.StatusBadRequest)
		}
		return true
	}
	if path != historyAPIPath {
		return false
	}
	runs, err := history.List()
	if err != nil {
		ctx.Error(err.Error(), fasthttp.StatusInternalServerError)
		return true
	}
	ctx.SetContentType("application/json")
	_ = json.NewEncoder(ctx).Encode(runs)
	return true
}

// historyPage shows the runs in the history, it's a raw string so there must be no backquote in it.
const historyPage = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>plow history</title>
<script src="/echarts/statics/echarts.min.js"></script>
<style>
.plow-history { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; color: #222; }
.plow-history .ph-layout { display: flex; gap: 24px; max-width: 1800px; margin: 0 auto; align-items: flex-start; }
.plow-history .ph-layout[hidden] { display: none; }
.plow-history .ph-sidebar { flex: 0 0 300px; position: sticky; top: 8px; max-height: calc(100vh - 16px); display: flex; flex-direction: column; }
.plow-history .ph-sidebar input { font-size: 13px; padding: 6px 8px; margin-bottom: 8px; border: 1px solid #ccc; border-radius: 4px; }
.plow-history .ph-groups { list-style: none; margin: 0; padding: 0; overflow-y: auto; }
.plow-history .ph-groups li { padding: 8px 10px; border-radius: 4px; cursor: pointer; }
.plow-history .ph-groups li:hover { background: #f3f3f3; }
.plow-history .ph-groups li.ph-active { background: #e8f0fe; }
.plow-history .ph-group-name { font-size: 13px; word-break: break-all; }
.plow-history .ph-group-meta { font-size: 12px; color: #888; margin-top: 2px; }
.plow-history .ph-main { flex: 1; min-width: 0; }
.plow-history .ph-header { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.plow-history .ph-group-title { margin: 8px 0 16px; word-break: break-all; }
.plow-history .ph-header button { font-size: 13px; padding: 4px 12px; cursor: pointer; }
.plow-history .ph-label { font-size: 12px; color: #0969da; background: #ddf4ff; border-radius: 10px; padding: 1px 8px; margin-top: 2px; width: fit-content; max-width: 200px; overflow: hidden; text-overflow: ellipsis; }
.plow-history .ph-truncate { display: inline-block; max-width: 480px; overflow: hidden; text-overflow: ellipsis; vertical-align: bottom; }
.plow-history .ph-charts { display: grid; grid-template-columns: repeat(auto-fit, minmax(480px, 1fr)); gap: 16px; margin-top: 16px; }
.plow-history .ph-chart { height: 320px; }
.plow-history .ph-section { margin-top: 24px; overflow-x: auto; }
.plow-history table { border-collapse: collapse; width: 100%; font-size: 13px; font-variant-numeric: tabular-nums; }
.plow-history th, .plow-history td { padding: 6px 10px; border-bottom: 1px solid #e5e5e5; text-align: right; white-space: nowrap; }
.plow-history th { background: #fafafa; font-weight: 600; }
.plow-history .ph-runs th, .plow-history .ph-runs td { padding: 6px 8px; }
.plow-history th:first-child, .plow-history td:first-child,
.plow-history .ph-runs th:nth-child(2), .plow-history .ph-runs td:nth-child(2) { text-align: left; }
.plow-history tr.ph-interrupted td { color: #999; }
.plow-history tr.ph-selected td { background: #fffbe6; }
.plow-history tr.ph-param td { color: #666; }
.plow-history tr.ph-differ td { background: #fff4d6; color: #9a6700; }
.plow-history .ph-tag { font-size: 12px; color: #b26a00; }
.plow-history .ph-delete { border: none; background: none; color: #999; font-size: 16px; padding: 0 4px; cursor: pointer; }
.plow-history .ph-delete:hover { color: #cf222e; }
.plow-history .ph-better { color: #1a7f37; font-weight: 600; }
.plow-history .ph-worse { color: #cf222e; font-weight: 600; }
.plow-history .ph-warn { color: #9a6700; margin: 8px 0; }
.plow-history .ph-warn:empty { display: none; }
.plow-history .ph-empty { text-align: center; color: #888; margin: 32px 0; }
.plow-history .ph-hint { color: #888; font-size: 13px; margin: 8px 0; }
</style>
</head>
<body>
<p align="center">🚀 <a href="https://github.com/six-ddc/plow"><b>Plow</b></a> History</p>
<div id="plow-history"></div>
<script>
// PlowHistory renders the runs in the history into root, grouped by "METHOD URL".
function PlowHistory(root) {
  const NOISE = 3;
  const PERCENTILES = [0.5, 0.75, 0.9, 0.95, 0.99, 0.999, 0.9999];
  const PHASES = ["DNS", "Connect", "TLS", "Write", "Wait", "Read"];
  const PALETTE = ["#5470c6", "#91cc75", "#fac858", "#ee6666", "#73c0de", "#3ba272", "#fc8452", "#9a60b4", "#ea7ccc"];
  let all = [];
  let group = null;
  let runs = [];
  // IDs of the selected runs
  const selected = new Set();
  // the newer selected run is compared with the older one by default
  let swapped = false;

  function el(tag, attrs, text) {
    const e = document.createElement(tag);
    Object.assign(e, attrs || {});
    if (text != null) e.textContent = text;
    return e;
  }

  function escape(s) {
    return String(s).replace(/[&<>"']/g, c => "&#" + c.charCodeAt(0) + ";");
  }

  function fmtDur(ns) {
    if (ns == null) return "-";
    if (ns < 1e3) return ns + "ns";
    if (ns < 1e6) return +(ns / 1e3).toFixed(1) + "µs";
    if (ns < 1e9) return +(ns / 1e6).toFixed(3) + "ms";
    return +(ns / 1e9).toFixed(3) + "s";
  }

  function fmtTime(t) {
    const d = new Date(t);
    const p = n => String(n).padStart(2, "0");
    return p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }

  function ms(ns) {
    return ns == null ? null : +(ns / 1e6).toFixed(3);
  }

  function percentile(run, p) {
    const item = (run.Result.Percentiles || []).find(x => Math.abs(x.Percentile - p) < 1e-9);
    return item ? item.Latency : null;
  }

  function errorRate(run) {
    const r = run.Result;
    if (!r.Count) return 0;
    const codes = r.Codes || {};
    const ok = (codes["2xx"] || 0) + (codes["3xx"] || 0);
    return (r.Count - ok) / r.Count * 100;
  }

  function limitText(run) {
    const o = run.Options;
    const parts = [];
    if (o.Duration > 0) parts.push(fmtDur(o.Duration));
    if (o.Requests > 0) parts.push("n=" + o.Requests);
    return parts.length ? parts.join(" / ") : "-";
  }

  // the options shown in the table, which tell whether two runs are comparable
  const PARAMS = [
    ["Concurrency", r => r.Options.Concurrency],
    ["Duration / Requests", limitText],
    ["Rate", r => r.Options.Rate || "-"],
    ["Ramp-up", r => r.Options.RampUp > 0 ? r.Options.RampUp : "-"],
    ["Trace", r => r.Options.Trace ? "on" : "-"],
    ["Request", r => r.Options.Fingerprint ? r.Options.Fingerprint.slice(0, 6) : "-",
      "Hash of the body, headers, timeouts and proxy"],
  ];

  function groupOf(run) {
    return run.Options.Method + " " + run.Options.URL;
  }

  function titleOf(run) {
    return fmtTime(run.Time);
  }

  const charts = new Map();
  window.addEventListener("resize", () => charts.forEach(c => c.resize()));

  function drawChart(container, option, onClick) {
    let chart = charts.get(container);
    if (!chart) {
      chart = echarts.init(container);
      if (onClick) chart.on("click", onClick);
      charts.set(container, chart);
    }
    chart.setOption(option, true);
    chart.resize();
  }

  function chartBox() {
    return el("div", { className: "ph-chart" });
  }

  function baseOption(title, extra) {
    return Object.assign({
      title: { text: title, textStyle: { fontSize: 14 } },
      tooltip: { trigger: "axis" },
      legend: { top: 24, type: "scroll" },
      grid: { top: 64, left: 56, right: 56, bottom: 32 },
    }, extra);
  }

  root.classList.add("plow-history");
  const notice = el("div", { className: "ph-warn", hidden: true });
  const empty = el("div", { className: "ph-empty", hidden: true });

  const filterInput = el("input", { type: "search", placeholder: "Filter URL", oninput: () => renderGroups() });
  const groupList = el("ul", { className: "ph-groups" });
  const sidebar = el("div", { className: "ph-sidebar" });
  sidebar.append(filterInput, groupList);

  const groupTitle = el("h3", { className: "ph-group-title" });
  const refreshButton = el("button", { title: "Load the runs finished since the page is opened", onclick: () => refresh() }, "Refresh");
  const header = el("div", { className: "ph-header" });
  header.append(groupTitle, refreshButton);
  const trendRPS = chartBox();
  const trendLatency = chartBox();
  const trend = el("div", { className: "ph-charts" });
  trend.append(trendRPS, trendLatency);

  const tbody = el("tbody");
  const table = el("table", { className: "ph-runs" });
  const head = el("tr");
  head.append(el("th"), el("th", null, "Time"));
  PARAMS.forEach(([name, , title]) => head.append(el("th", { title: title || "" }, name)));
  ["Count", "RPS", "Mean", "P50", "P90", "P99", "Max", "Error Rate", ""].forEach(h => head.append(el("th", null, h)));
  table.append(el("thead"), tbody);
  table.tHead.append(head);
  const hint = el("div", { className: "ph-hint" });
  const tableSection = el("div", { className: "ph-section" });
  tableSection.append(hint, table);

  const comparison = el("div", { className: "ph-section", hidden: true });
  const compareTitle = el("h3");
  const swapButton = el("button", { onclick: () => { swapped = !swapped; render(); } }, "⇄ Swap baseline");
  const compareHeader = el("div", { className: "ph-header" });
  compareHeader.append(compareTitle, swapButton);
  const compareNote = el("div", { className: "ph-hint" });
  const compareWarn = el("div", { className: "ph-warn" });
  const compareTable = el("table");
  comparison.append(compareHeader, compareWarn, compareTable, compareNote);

  // the charts of the realtime page replayed from the data of each second
  const replay = el("div", { className: "ph-section", hidden: true });
  const replayTitle = el("h3");
  const seriesLatency = chartBox();
  const seriesRPS = chartBox();
  const seriesCodes = chartBox();
  const seriesConcurrency = chartBox();
  const seriesPhases = chartBox();
  const percentiles = chartBox();
  const seriesNote = el("div", { className: "ph-hint" });
  const replayCharts = el("div", { className: "ph-charts" });
  replayCharts.append(seriesLatency, seriesRPS, seriesCodes, seriesConcurrency, seriesPhases, percentiles);
  replay.append(replayTitle, seriesNote, replayCharts);

  const main = el("div", { className: "ph-main" });
  main.append(header, trend, tableSection, comparison, replay);
  const layout = el("div", { className: "ph-layout", hidden: true });
  layout.append(sidebar, main);
  root.append(notice, empty, layout);

  function selectGroup(name) {
    if (name === group) return;
    group = name;
    selected.clear();
    swapped = false;
    history.replaceState(null, "", "#" + encodeURIComponent(name));
    runs = all.filter(r => groupOf(r) === group);
    render();
  }

  function renderGroups() {
    const groups = new Map();
    // the runs are sorted from the newest
    all.forEach(r => {
      const name = groupOf(r);
      const g = groups.get(name) || { name, count: 0, latest: r.Time };
      g.count++;
      groups.set(name, g);
    });
    const filter = filterInput.value.trim().toLowerCase();
    groupList.replaceChildren();
    groups.forEach(g => {
      if (filter && !g.name.toLowerCase().includes(filter)) return;
      const li = el("li", { className: g.name === group ? "ph-active" : "", title: g.name, onclick: () => selectGroup(g.name) });
      li.append(el("div", { className: "ph-group-name" }, g.name),
        el("div", { className: "ph-group-meta" }, g.count + (g.count > 1 ? " runs" : " run") + " · " + fmtTime(g.latest)));
      groupList.append(li);
    });
  }

  function toggle(key) {
    selected.has(key) ? selected.delete(key) : selected.add(key);
    swapped = false;
    render();
  }

  function renderTable(list) {
    tbody.replaceChildren();
    list.forEach(run => {
      const key = run.ID;
      const classes = [];
      if (run.Interrupted) classes.push("ph-interrupted");
      if (selected.has(key)) classes.push("ph-selected");
      const tr = el("tr", { className: classes.join(" ") });
      const check = el("td");
      check.append(el("input", { type: "checkbox", checked: selected.has(key), onchange: () => toggle(key) }));
      tr.append(check);
      const time = el("td", null, titleOf(run));
      if (run.Interrupted) time.append(" ", el("span", { className: "ph-tag" }, "interrupted"));
      if (run.Label) time.append(el("div", { className: "ph-label", title: run.Label }, run.Label));
      tr.append(time);
      [
        ...PARAMS.map(([, get]) => get(run)),
        run.Result.Count,
        run.Result.RPS.toFixed(2),
        fmtDur(run.Result.Latency.Mean),
        fmtDur(percentile(run, 0.5)),
        fmtDur(percentile(run, 0.9)),
        fmtDur(percentile(run, 0.99)),
        fmtDur(run.Result.Latency.Max),
        errorRate(run).toFixed(2) + "%",
      ].forEach(v => tr.append(el("td", null, v)));
      const del = el("td");
      del.append(el("button", { className: "ph-delete", title: "Delete this run", onclick: () => deleteRun(run) }, "×"));
      tr.append(del);
      tbody.append(tr);
    });
  }

  // the keys of the runs in the trend charts, by the index of the points
  let trendKeys = [];
  const clickTrend = p => toggle(trendKeys[p.dataIndex]);

  function renderTrend(list) {
    const ordered = [...list].reverse();
    trendKeys = ordered.map(r => r.ID);
    const labels = ordered.map(titleOf);
    const point = (run, value) => ({ value, symbolSize: selected.has(run.ID) ? 12 : 5 });
    const series = (name, get, extra) => Object.assign({ name, type: "line", data: ordered.map(r => point(r, get(r))) }, extra);
    const tooltip = fmt => ({
      trigger: "axis",
      confine: true,
      formatter: ps => {
        const run = ordered[ps[0].dataIndex];
        return ["<b>" + escape(labels[ps[0].dataIndex]) + "</b>", ...(run.Label ? [escape(run.Label)] : []), ...PARAMS.map(([name, get]) => escape(name) + ": " + escape(get(run))), "",
          ...ps.map(p => p.marker + escape(p.seriesName) + ": " + fmt(p.seriesName, p.value))].join("<br>");
      },
    });
    const xAxis = { type: "category", data: labels };
    drawChart(trendRPS, baseOption("RPS & Error Rate of each run", {
      tooltip: tooltip((name, v) => name === "RPS" ? v.toFixed(2) : v.toFixed(2) + "%"),
      xAxis,
      yAxis: [{ type: "value", name: "RPS", scale: true }, { type: "value", name: "Error %", min: 0, splitLine: { show: false } }],
      series: [
        series("RPS", r => r.Result.RPS),
        series("Error Rate", errorRate, { yAxisIndex: 1 }),
      ],
    }), clickTrend);
    drawChart(trendLatency, baseOption("Latency of each run", {
      tooltip: tooltip((name, v) => v == null ? "-" : v + "ms"),
      xAxis,
      yAxis: { type: "value", axisLabel: { formatter: "{value} ms" }, scale: true },
      series: [0.5, 0.9, 0.99].map(p => series("P" + p * 100, r => ms(percentile(r, p)))),
    }), clickTrend);
  }

  // lowerBetter: whether a smaller value is better, null when neither is; pp: compare by the difference of percentage points
  function changeCell(base, cur, lowerBetter, pp) {
    const td = el("td");
    if (base == null || cur == null) {
      td.textContent = "-";
      return td;
    }
    let diff, text;
    if (pp) {
      diff = cur - base;
      text = (diff >= 0 ? "+" : "") + diff.toFixed(2) + "pp";
    } else if (base === 0) {
      diff = cur === 0 ? 0 : Infinity;
      text = cur === 0 ? "0%" : "+∞";
    } else {
      diff = (cur - base) / base * 100;
      text = (diff >= 0 ? "+" : "") + diff.toFixed(1) + "%";
    }
    const significant = pp ? Math.abs(diff) >= 0.01 : Math.abs(diff) >= NOISE;
    if (significant && lowerBetter != null) td.className = (diff < 0) === lowerBetter ? "ph-better" : "ph-worse";
    td.textContent = text;
    return td;
  }

  function statusOf(run) {
    return run.Interrupted ? "interrupted" : "completed";
  }

  // the share (%) of the requests of each status code class and error, shown with the count
  function shareRows(a, b) {
    const rows = [];
    const share = (count, r) => r.Result.Count ? count / r.Result.Count * 100 : 0;
    const fmt = (get) => (v, r) => v.toFixed(2) + "% (" + get(r) + ")";
    const codes = new Set([...Object.keys(a.Result.Codes || {}), ...Object.keys(b.Result.Codes || {})]);
    [...codes].sort().forEach(code => {
      const count = r => (r.Result.Codes || {})[code] || 0;
      rows.push([code, r => share(count(r), r), fmt(count), code !== "2xx" && code !== "3xx", true]);
    });
    const errors = new Map();
    [a, b].forEach(r => Object.entries(r.Result.Errors || {}).forEach(([e, n]) => errors.set(e, (errors.get(e) || 0) + n)));
    [...errors.entries()].sort((x, y) => y[1] - x[1]).slice(0, 5).forEach(([e]) => {
      const count = r => (r.Result.Errors || {})[e] || 0;
      rows.push([e, r => share(count(r), r), fmt(count), true, true, "Error"]);
    });
    return rows;
  }

  function renderCompareTable(a, b) {
    // [name, get, format, lowerBetter, pp, prefix]
    const rows = [
      ["RPS", r => r.Result.RPS, v => v.toFixed(2), false],
      ["Error Rate", errorRate, v => v.toFixed(2) + "%", true, true],
      ...shareRows(a, b),
      ["Latency Mean", r => r.Result.Latency.Mean, fmtDur, true],
      ["P50", r => percentile(r, 0.5), fmtDur, true],
      ["P90", r => percentile(r, 0.9), fmtDur, true],
      ["P99", r => percentile(r, 0.99), fmtDur, true],
      ["P99.9", r => percentile(r, 0.999), fmtDur, true],
      ["Max", r => r.Result.Latency.Max, fmtDur, true],
      ["Read", r => r.Result.ReadThroughput, v => v.toFixed(2) + " MB/s", null],
      ["Write", r => r.Result.WriteThroughput, v => v.toFixed(2) + " MB/s", null],
    ];
    const phases = r => Object.fromEntries((r.Result.Phases || []).filter(p => p.Count > 0).map(p => [p.Name, p]));
    const pa = phases(a), pb = phases(b);
    Object.keys(pa).filter(n => pb[n]).forEach(name => {
      const p99 = p => p.Percentiles.find(x => Math.abs(x.Percentile - 0.99) < 1e-9);
      rows.push([name + " Mean", r => (r === a ? pa : pb)[name].Mean, fmtDur, true]);
      rows.push([name + " P99", r => { const v = p99((r === a ? pa : pb)[name]); return v ? v.Latency : null; }, fmtDur, true]);
    });

    compareTable.replaceChildren();
    const h = el("tr");
    ["", "Baseline", "Compared", "Change"].forEach(x => h.append(el("th", null, x)));
    compareTable.appendChild(el("thead")).append(h);
    const body = compareTable.appendChild(el("tbody"));
    const info = (name, get, highlight) => {
      const va = get(a), vb = get(b);
      const tr = el("tr", { className: "ph-param" + (highlight && va !== vb ? " ph-differ" : "") });
      tr.append(el("td", null, name), el("td", null, va), el("td", null, vb), el("td"));
      body.append(tr);
    };
    PARAMS.forEach(([name, get]) => info(name, get, true));
    info("Status", statusOf, true);
    info("Elapsed", r => fmtDur(r.Result.Elapsed), false);
    info("Requests", r => r.Result.Count, false);
    rows.forEach(([name, get, fmt, lowerBetter, pp, prefix]) => {
      const va = get(a), vb = get(b);
      const tr = el("tr");
      const label = prefix ? prefix + ": " + name : name;
      const nameCell = el("td");
      nameCell.append(el("span", { className: "ph-truncate", title: label }, label));
      tr.append(nameCell,
        el("td", null, va == null ? "-" : fmt(va, a)), el("td", null, vb == null ? "-" : fmt(vb, b)),
        changeCell(va, vb, lowerBetter, pp));
      body.append(tr);
    });
  }

  // names prefix the series of each run, the charts are the same as the realtime page
  function renderReplay(pair, names) {
    const point = (r, get) => (r.Series || []).map(p => [+(p.Elapsed / 1e9).toFixed(3), get(p)]);
    // the same metric of the runs has the same color, and the compared run is dashed
    const style = (i, key, colors) => {
      if (!colors.has(key)) colors.set(key, PALETTE[colors.size % PALETTE.length]);
      return { color: colors.get(key), lineStyle: { type: i ? "dashed" : "solid" } };
    };
    const line = (r, i, name, get) => ({ name: names[i] + name, key: name, run: i, type: "line", smooth: true, data: point(r, get) });
    const each = f => pair.flatMap((r, i) => f(r, i));
    const chart = (box, title, series, unit, unselected) => {
      const colors = new Map();
      series.forEach(x => Object.assign(x, style(x.run, x.key, colors)));
      drawChart(box, baseOption(title, {
        xAxis: { type: "value", name: "s", min: 0 },
        yAxis: { type: "value", scale: true, axisLabel: { formatter: "{value}" + unit } },
        legend: { top: 24, type: "scroll", selected: Object.fromEntries(series.filter(x => unselected && unselected.test(x.name)).map(x => [x.name, false])) },
        series,
      }));
    };
    const latency = get => p => p.Count > 0 ? ms(get(p)) : null;

    chart(seriesLatency, "Latency", each((r, i) => [
      line(r, i, "Min", latency(p => p.Min)),
      line(r, i, "Mean", latency(p => p.Mean)),
      line(r, i, "Max", latency(p => p.Max)),
    ]), " ms", /(Min|Max)$/);
    chart(seriesRPS, "Reqs/sec", each((r, i) => [line(r, i, "RPS", p => p.Count > 0 ? +p.RPS.toFixed(2) : null)]), "");
    const codesOf = r => [...new Set((r.Series || []).flatMap(p => Object.keys(p.Codes || {})))].sort();
    chart(seriesCodes, "Response Status", each((r, i) => codesOf(r).map(c => line(r, i, c, p => (p.Codes || {})[c] || 0))), "");
    chart(seriesConcurrency, "Concurrency", each((r, i) => [line(r, i, "Concurrency", p => p.Concurrency)]), "");
    seriesPhases.hidden = !pair.some(r => (r.Series || []).some(p => p.Phases));
    if (!seriesPhases.hidden) {
      chart(seriesPhases, "Latency Phases (Mean)", each((r, i) => !(r.Series || []).some(p => p.Phases) ? [] :
        PHASES.map((name, j) => line(r, i, name, p => p.Phases && p.Phases[j] > 0 ? ms(p.Phases[j]) : null))), " ms");
    }
    const missing = pair.filter(r => !(r.Series && r.Series.length)).map(titleOf);
    seriesNote.textContent = missing.length ? "No data of each second of " + missing.join(", ") + ", which runs less than a second or is recorded by an older plow." : "";

    drawChart(percentiles, baseOption("Latency Percentiles", {
      xAxis: { type: "category", data: PERCENTILES.map(p => "P" + +(p * 100).toFixed(2)) },
      yAxis: { type: "value", axisLabel: { formatter: "{value} ms" } },
      series: pair.map((r, i) => Object.assign({ name: names[i] + "Latency", type: "line", data: PERCENTILES.map(p => ms(percentile(r, p))) }, style(i, "Latency", new Map()))),
    }));
  }

  function runTitle(r) {
    return titleOf(r) + (r.Label ? " (" + r.Label + ")" : "");
  }

  function renderSelection(list) {
    const pair = list.filter(r => selected.has(r.ID));
    comparison.hidden = pair.length !== 2;
    replay.hidden = pair.length < 1 || pair.length > 2;
    hint.textContent = pair.length === 0 ? "Select a run to see its charts, or two runs to compare them"
      : pair.length > 2 ? "Select at most two runs to compare them" : "";
    if (pair.length === 1) {
      replayTitle.textContent = "Run " + runTitle(pair[0]);
      renderReplay(pair, [""]);
      return;
    }
    if (pair.length !== 2) return;
    // the list is from the newest, the older one is the baseline unless swapped
    const [b, a] = swapped ? [...pair].reverse() : pair;
    compareTitle.textContent = "Baseline " + runTitle(a) + "  →  Compared " + runTitle(b);
    const diffs = PARAMS.filter(([, get]) => get(a) !== get(b)).map(([name]) => name);
    compareWarn.textContent = diffs.length ? "⚠ The runs have different " + diffs.join(", ") + ", the results may be not comparable." : "";
    const notes = ["Changes over 3% are highlighted, but a single run may vary more than that, run it a few times before drawing a conclusion."];
    if (b.Result.RPS > a.Result.RPS && errorRate(b) > errorRate(a)) notes.push("Higher RPS with more errors may come from the requests failing faster.");
    compareNote.textContent = notes.join(" ");
    renderCompareTable(a, b);
    replayTitle.textContent = "Charts";
    renderReplay([a, b], ["Baseline ", "Compared "]);
  }

  function render() {
    renderGroups();
    const list = runs;
    if (!list.length) {
      empty.textContent = "No runs recorded yet, run plow to record one.";
      empty.hidden = false;
      layout.hidden = true;
      return;
    }
    empty.hidden = true;
    layout.hidden = false;
    groupTitle.textContent = group;
    renderTable(list);
    renderTrend(list);
    renderSelection(list);
  }

  async function deleteRun(run) {
    if (!confirm("Delete the run at " + fmtTime(run.Time) + "?")) return;
    const resp = await fetch("/history/api/runs/" + encodeURIComponent(run.ID), { method: "DELETE" });
    if (!resp.ok) {
      alert("Failed to delete: " + await resp.text());
      return;
    }
    selected.delete(run.ID);
    await load();
  }

  async function load() {
    const resp = await fetch("/history/api/runs");
    if (!resp.ok) throw new Error(await resp.text());
    all = await resp.json();
    if (!all.some(r => groupOf(r) === group)) {
      const fromHash = decodeURIComponent(location.hash.slice(1));
      group = all.some(r => groupOf(r) === fromHash) ? fromHash : all.length ? groupOf(all[0]) : null;
      selected.clear();
      swapped = false;
    }
    runs = all.filter(r => groupOf(r) === group);
    render();
  }

  async function refresh() {
    refreshButton.disabled = true;
    try {
      await load();
      notice.hidden = true;
    } catch (e) {
      notice.textContent = "⚠ Failed to load the history: " + e.message;
      notice.hidden = false;
    } finally {
      refreshButton.disabled = false;
    }
  }

  load().catch(e => {
    notice.textContent = "⚠ Failed to load the history: " + e.message;
    notice.hidden = false;
  });
}

PlowHistory(document.getElementById("plow-history"));
</script>
</body>
</html>
`
