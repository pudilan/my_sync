(function () {
  "use strict";
  var pill = document.getElementById("run-pill");
  var statusEl = document.getElementById("run-status");
  var logEl = document.getElementById("run-log");
  var previewEl = document.getElementById("preview");
  var cancelBtn = document.getElementById("cancel-btn");
  var runBtns = document.querySelectorAll("[data-run]");
  var wasRunning = null;
  var lastLogLen = -1;

  var phases = { scanning: "扫描文件", comparing: "比对增量", archiving: "写入压缩包", finalizing: "写入数据库" };
  var ops = { added: "新增", modified: "修改", deleted: "删除" };
  var skips = { deferred: "延后", oversize: "超限" };

  function leftover(r) {
    var h = "";
    if (r.deferred) h += '<div class="warn">' + r.deferred + " 个文件 (" + size(r.deferred_bytes) + ") 超出单次备份上限，留到下次备份</div>";
    if (r.oversize) h += '<div class="warn">' + r.oversize + " 个文件单个就超过单次备份上限，无法备份，请调大上限</div>";
    return h;
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function size(n) {
    if (n < 1024) return n + " B";
    var u = "KMGTP", i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < 4);
    return n.toFixed(2) + " " + u[i] + "B";
  }
  function ftime(s) {
    if (!s || s.indexOf("0001-") === 0) return "-";
    var d = new Date(s);
    var p = function (x) { return (x < 10 ? "0" : "") + x; };
    return d.getFullYear() + "-" + p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }

  function renderRun(run) {
    if (!run) { statusEl.innerHTML = '<span class="muted">本次启动后尚未执行任务。</span>'; return; }
    var kind = run.dry_run ? "预览" : "备份";
    var h = "";
    if (run.running) {
      var p = run.progress || {};
      var pct = 0;
      if (p.bytes_total > 0) pct = Math.min(100, p.bytes_done * 100 / p.bytes_total);
      else if (p.files_total > 0) pct = Math.min(100, p.files_done * 100 / p.files_total);
      h += "<div><b>" + kind + "进行中</b> · " + esc(phases[p.phase] || p.phase || "准备中") + "</div>";
      h += '<div class="progress"><div style="width:' + pct.toFixed(1) + '%"></div></div>';
      if (p.phase === "archiving") {
        h += "<div>" + p.files_done + " / " + p.files_total + " 个文件 · " + size(p.bytes_done) + " / " + size(p.bytes_total) + "</div>";
        if (p.current) h += '<div class="muted path">' + esc(p.current) + "</div>";
      }
    } else if (run.error) {
      h += '<div class="warn"><b>' + kind + "失败</b>: " + esc(run.error) + "</div>";
    } else if (run.result) {
      var r = run.result;
      if (r.no_changes) {
        h += "<div><b>" + kind + "完成</b>: 没有增量内容，未生成压缩包和数据库。</div>";
      } else if (r.dry_run) {
        h += "<div><b>预览完成</b>: 本次将新增 " + r.added + "、修改 " + r.modified + "、删除 " + r.deleted + "，待归档 " + size(r.bytes) + "</div>" + leftover(r);
      } else {
        h += "<div><b>备份完成</b>: <a href=\"/backups/" + esc(r.backup_id) + "\">" + esc(r.backup_id) + "</a> · 新增 " + r.added + "、修改 " + r.modified + "、删除 " + r.deleted + " · " + size(r.bytes) + "</div>" + leftover(r);
      }
    }
    h += '<div class="muted result">开始 ' + ftime(run.started_at) + (run.running ? "" : " · 结束 " + ftime(run.finished_at)) + "</div>";
    statusEl.innerHTML = h;
  }

  function renderPreview(run) {
    if (!previewEl) return;
    var r = run && run.result;
    if (!r || !r.dry_run || !r.changes || !r.changes.length) { previewEl.innerHTML = ""; return; }
    var total = r.added + r.modified + r.deleted + r.deferred + r.oversize;
    var h = "<h3>增量预览" + (total > r.changes.length ? "（仅显示前 " + r.changes.length + " 条，共 " + total + " 条）" : "") + "</h3>";
    h += '<div class="table-wrap"><table><thead><tr><th>类型</th><th>路径</th><th>大小</th></tr></thead><tbody>';
    r.changes.forEach(function (c) {
      h += '<tr><td><span class="tag op-' + c.op + '">' + (ops[c.op] || c.op) + "</span>" + (c.skip ? ' <span class="tag warn">' + skips[c.skip] + "</span>" : "") + '</td><td class="path">' + esc(c.path) + '</td><td class="num">' + (c.size ? size(c.size) : "") + "</td></tr>";
    });
    previewEl.innerHTML = h + "</tbody></table></div>";
  }

  function update(st) {
    var run = st.current;
    var running = !!(run && run.running);
    pill.textContent = running ? "备份中…" : "空闲";
    pill.classList.toggle("busy", running);
    if (statusEl) {
      renderRun(run);
      runBtns.forEach(function (b) { b.disabled = running; });
      cancelBtn.hidden = !running;
      var logs = (run && run.logs) || [];
      if (logs.length !== lastLogLen) {
        lastLogLen = logs.length;
        var atBottom = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 20;
        logEl.textContent = logs.map(function (l) { return ftime(l.time) + "  " + l.msg; }).join("\n") || "暂无日志";
        if (atBottom) logEl.scrollTop = logEl.scrollHeight;
      }
      if (!running) renderPreview(run);
      if (wasRunning && !running && run && run.result && run.result.backup_id) {
        setTimeout(function () { location.reload(); }, 800);
      }
    }
    wasRunning = running;
  }

  var timer = null;
  function schedule(ms) { clearTimeout(timer); timer = setTimeout(poll, ms); }
  function poll() {
    clearTimeout(timer);
    fetch("/api/status", { credentials: "same-origin" })
      .then(function (r) { return r.json(); })
      .then(function (st) { update(st); schedule(wasRunning ? 1000 : 5000); })
      .catch(function () { schedule(5000); });
  }

  function post(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body || ""
    }).then(function (r) { return r.json(); });
  }

  runBtns.forEach(function (b) {
    b.addEventListener("click", function () {
      runBtns.forEach(function (x) { x.disabled = true; });
      post("/api/run", "dry_run=" + b.getAttribute("data-run")).then(function (res) {
        if (res.error) alert(res.error);
        if (!res.error) wasRunning = true;
        lastLogLen = -1;
        poll();
      });
    });
  });
  if (cancelBtn) {
    cancelBtn.addEventListener("click", function () {
      if (confirm("确定取消当前任务？未完成的备份不会保留。")) post("/api/cancel");
    });
  }
  poll();
})();
