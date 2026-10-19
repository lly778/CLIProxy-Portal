(function () {
  "use strict";
  var panel = document.querySelector("[data-resource-history]");
  if (!panel) return;
  var svgNS = "http://www.w3.org/2000/svg";
  var status = panel.querySelector("[data-resource-history-status]");
  var snapshot = null;
  var busy = false;

  function node(tag, attrs, text) {
    var element = document.createElementNS(svgNS, tag);
    Object.keys(attrs || {}).forEach(function (key) { element.setAttribute(key, attrs[key]); });
    if (text !== undefined) element.textContent = text;
    return element;
  }

  function clock(at, seconds) {
    var options = {hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: snapshot.zone};
    if (seconds) options.second = "2-digit";
    return new Intl.DateTimeFormat("zh-CN", options).format(at);
  }

  function normalize(raw) {
    var from = Date.parse(raw.from), to = Date.parse(raw.to);
    if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from || to - from > 3601000 || !Array.isArray(raw.points)) throw new Error("history");
    var points = raw.points.slice(-361).map(function (p) {
      return {at: Date.parse(p.at), cpu: p.cpu_percent, memory: p.used_mib, total: p.total_mib};
    }).filter(function (p) {
      return Number.isFinite(p.at) && p.at >= from && p.at <= to && Number.isFinite(p.cpu) && p.cpu >= 0 && p.cpu <= 100 && Number.isFinite(p.memory) && Number.isFinite(p.total) && p.total > 0 && p.memory >= 0 && p.memory <= p.total;
    }).sort(function (a, b) { return a.at - b.at; });
    var zone = raw.time_zone || "Asia/Shanghai";
    // Validate the server's zone before it reaches the drawing callbacks.
    new Intl.DateTimeFormat("zh-CN", {timeZone: zone}).format(to);
    return {from: from, to: to, zone: zone, interval: 10000, points: points};
  }

  function Chart(card) {
    var stage = card.querySelector(".resource-chart-stage");
    var svg = card.querySelector("svg");
    var tooltip = card.querySelector(".resource-chart-tooltip");
    var points = [], width = 0, left = 43, right = 43, top = 13, bottom = 213;
    var memoryMax = 1, cursor = null, dots = {}, selected = -1;

    function x(p) { return left + (p.at - snapshot.from) / (snapshot.to - snapshot.from) * (width - left - right); }
    function y(p, key) { return bottom - p[key] / (key === "cpu" ? 100 : memoryMax) * (bottom - top); }

    function hide() {
      tooltip.hidden = true;
      if (cursor) cursor.setAttribute("visibility", "hidden");
      Object.keys(dots).forEach(function (key) { dots[key].setAttribute("visibility", "hidden"); });
    }

    function select(index) {
      if (!points.length || !cursor) return;
      selected = Math.max(0, Math.min(points.length - 1, index));
      var p = points[selected], px = x(p);
      cursor.setAttribute("x1", px); cursor.setAttribute("x2", px); cursor.setAttribute("visibility", "visible");
      Object.keys(dots).forEach(function (key) {
        dots[key].setAttribute("cx", px); dots[key].setAttribute("cy", y(p, key)); dots[key].setAttribute("visibility", "visible");
      });
      var at = document.createElement("time");
      at.dateTime = new Date(p.at).toISOString(); at.textContent = clock(p.at, true) + " · 10 秒峰值";
      var cpu = document.createElement("strong"); cpu.textContent = "CPU " + p.cpu.toFixed(1) + "%";
      var memory = document.createElement("strong"); memory.textContent = "内存 " + Math.round(p.memory) + " MiB";
      tooltip.replaceChildren(at, cpu, memory); tooltip.hidden = false;
      tooltip.style.left = Math.max(8, Math.min(width - tooltip.offsetWidth - 8, px - tooltip.offsetWidth / 2)) + "px";
    }

    function draw() {
      if (!snapshot) return;
      hide(); points = snapshot.points; selected = -1;
      width = Math.max(250, stage.clientWidth);
      memoryMax = Math.max(1, points.reduce(function (n, p) { return Math.max(n, p.total); }, 0));
      svg.setAttribute("viewBox", "0 0 " + width + " 245");
      var fragment = document.createDocumentFragment();
      for (var i = 0; i <= 4; i++) {
        var py = bottom - i / 4 * (bottom - top);
        fragment.appendChild(node("line", {x1: left, x2: width - right, y1: py, y2: py, "class": "resource-chart-grid"}));
        fragment.appendChild(node("text", {x: left - 8, y: py + 4, "text-anchor": "end"}, Math.round(i * 100 / 4) + "%"));
        fragment.appendChild(node("text", {x: width - right + 8, y: py + 4, "text-anchor": "start"}, points.length ? Math.round(i * memoryMax / 4) : "—"));
      }
      var ticks = width < 380 ? 2 : 4;
      for (var j = 0; j <= ticks; j++) {
        var px = left + j / ticks * (width - left - right);
        fragment.appendChild(node("text", {x: px, y: bottom + 23, "text-anchor": j === 0 ? "start" : (j === ticks ? "end" : "middle")}, clock(snapshot.from + j / ticks * (snapshot.to - snapshot.from), false)));
      }
      // Gaps in monitoring remain gaps instead of implying an interpolated load.
      var segments = [], segment = [];
      points.forEach(function (p) {
        if (segment.length && p.at - segment[segment.length - 1].at > snapshot.interval * 2.5) { segments.push(segment); segment = []; }
        segment.push(p);
      });
      if (segment.length) segments.push(segment);
      var peakLabels = [];
      dots = {};
      ["cpu", "memory"].forEach(function (key) {
        var group = node("g", {"class": "resource-chart-" + key + "-series"});
        var linePath = "", areaPath = "";
        segments.forEach(function (part) {
          var path = part.map(function (p, k) { return (k ? "L" : "M") + x(p).toFixed(2) + " " + y(p, key).toFixed(2); }).join(" ");
          linePath += path + " ";
          areaPath += "M" + x(part[0]).toFixed(2) + " " + bottom + " " + path.replace(/^M/, "L") + " L" + x(part[part.length - 1]).toFixed(2) + " " + bottom + " Z ";
        });
        if (points.length) {
          group.appendChild(node("path", {d: areaPath, "class": "resource-chart-area"}));
          group.appendChild(node("path", {d: linePath, "class": "resource-chart-line"}));
          var latest = points[points.length - 1];
          group.appendChild(node("circle", {cx: x(latest), cy: y(latest, key), r: 2.5, "class": "resource-chart-point"}));
        }
        dots[key] = node("circle", {r: 3.5, "class": "resource-chart-point", visibility: "hidden"});
        group.appendChild(dots[key]); fragment.appendChild(group);
        var peak = points.reduce(function (n, p) { return Math.max(n, p[key]); }, 0);
        var peakText = points.length ? (key === "cpu" ? peak.toFixed(1) + "%" : Math.round(peak) + " MiB") : "—";
        card.querySelector('[data-resource-chart-peak="' + key + '"]').textContent = "峰值 " + peakText;
        peakLabels.push((key === "cpu" ? "CPU " : "内存 ") + peakText);
      });
      cursor = node("line", {y1: top, y2: bottom, "class": "resource-chart-cursor", visibility: "hidden"});
      fragment.appendChild(cursor); svg.replaceChildren(fragment);
      card.querySelector("[data-resource-chart-empty]").hidden = points.length > 0;
      svg.setAttribute("aria-label", "最近六十分钟 CPU 和内存趋势，峰值 " + peakLabels.join("，"));
    }

    stage.addEventListener("pointermove", function (event) {
      if (!points.length || !snapshot) return;
      var px = event.clientX - stage.getBoundingClientRect().left;
      var target = snapshot.from + (px - left) / (width - left - right) * (snapshot.to - snapshot.from);
      var closest = 0;
      for (var i = 1; i < points.length; i++) if (Math.abs(points[i].at - target) < Math.abs(points[closest].at - target)) closest = i;
      if (Math.abs(points[closest].at - target) > snapshot.interval * 2.5) hide();
      else select(closest);
    });
    stage.addEventListener("pointerleave", hide);
    stage.addEventListener("blur", hide);
    stage.addEventListener("focus", function () { select(points.length - 1); });
    stage.addEventListener("keydown", function (event) {
      if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
      event.preventDefault(); select((selected < 0 ? points.length - 1 : selected) + (event.key === "ArrowLeft" ? -1 : 1));
    });
    return {draw: draw};
  }

  var charts = Array.from(panel.querySelectorAll("[data-resource-chart]"), function (card) { return new Chart(card); });
  function refresh() {
    if (busy || document.visibilityState === "hidden") return;
    busy = true;
    var abort = new AbortController();
    var timeout = window.setTimeout(function () { abort.abort(); }, 8000);
    fetch("/admin/system/resources/history", {headers: {Accept: "application/json"}, credentials: "same-origin", cache: "no-store", signal: abort.signal})
      .then(function (response) { if (!response.ok || !(response.headers.get("content-type") || "").includes("application/json")) throw new Error("history"); return response.json(); })
      .then(function (raw) { snapshot = normalize(raw); charts.forEach(function (chart) { chart.draw(); }); status.hidden = true; })
      .catch(function () { status.textContent = snapshot ? "趋势暂时无法刷新，显示上次数据。" : "趋势暂时无法加载，请稍后重试。"; status.hidden = false; })
      .finally(function () { window.clearTimeout(timeout); busy = false; });
  }
  refresh();
  window.setInterval(refresh, 15000);
  document.addEventListener("visibilitychange", function () { if (document.visibilityState !== "hidden") refresh(); });
  var resizeTimer;
  window.addEventListener("resize", function () { window.clearTimeout(resizeTimer); resizeTimer = window.setTimeout(function () { charts.forEach(function (chart) { chart.draw(); }); }, 150); });
}());
