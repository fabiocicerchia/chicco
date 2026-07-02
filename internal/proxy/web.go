package proxy

import (
	"net/http"
)

// handleDashboard serves the self-contained HTML web dashboard that mirrors the
// TUI. The page polls /v1/status every second and renders the provider table and
// log pane in the browser without any external dependencies.
func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(dashboardHTML))
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>chicco dashboard</title>
<style>
  :root {
    --bg: #1e2127;
    --bg2: #282c34;
    --bg3: #2c313a;
    --border: #4b5263;
    --green: #2ecc71;
    --amber: #f1c40f;
    --red:   #e74c3c;
    --grey:  #7f8c8d;
    --dim:   #5c6370;
    --title: #61afef;
    --fg:    #abb2bf;
  }
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    background: var(--bg);
    color: var(--fg);
    font-family: 'Fira Code', 'Cascadia Code', 'JetBrains Mono', ui-monospace, monospace;
    font-size: 13px;
    line-height: 1.4;
    height: 100vh;
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 8px;
    overflow: hidden;
  }

  /* ── panel chrome ── */
  .panel {
    border: 1px solid var(--border);
    border-radius: 6px;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }
  .panel-header {
    padding: 4px 10px;
    border-bottom: 1px solid var(--border);
    background: var(--bg2);
    color: var(--title);
    font-weight: bold;
    display: flex;
    align-items: center;
    gap: 8px;
    white-space: nowrap;
    overflow: hidden;
  }
  .panel-header .sub {
    color: var(--dim);
    font-weight: normal;
    font-size: 0.9em;
  }
  .panel-body {
    overflow: auto;
    flex: 1;
    background: var(--bg);
  }

  /* ── provider panel ── */
  #providers-panel { flex: 3; min-height: 0; }

  table {
    width: 100%;
    border-collapse: collapse;
  }
  thead th {
    position: sticky;
    top: 0;
    background: var(--bg2);
    color: var(--dim);
    font-weight: bold;
    text-align: left;
    padding: 4px 8px;
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }
  tbody tr:nth-child(even) { background: var(--bg3); }
  tbody tr:hover { background: var(--bg2); }
  td {
    padding: 3px 8px;
    white-space: nowrap;
    vertical-align: middle;
  }
  td.model-cont { color: var(--dim); padding-left: 28px; }

  /* ── status dot ── */
  .dot {
    display: inline-block;
    width: 10px;
    height: 10px;
    border-radius: 50%;
    flex-shrink: 0;
  }
  .dot-green  { background: var(--green); }
  .dot-amber  { background: var(--amber); }
  .dot-grey   { background: var(--grey);  }
  .dot-hollow {
    background: transparent;
    border: 2px solid var(--grey);
    width: 8px; height: 8px;
  }

  /* ── usage bar ── */
  .bar-wrap {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 120px;
  }
  .bar-track {
    flex: 1;
    height: 8px;
    background: var(--bg3);
    border-radius: 4px;
    overflow: hidden;
    min-width: 60px;
    max-width: 200px;
  }
  .bar-fill {
    height: 100%;
    border-radius: 4px;
    transition: width 0.4s ease;
  }
  .bar-fill.green { background: var(--green); }
  .bar-fill.amber { background: var(--amber); }
  .bar-fill.red   { background: var(--red);   }
  .bar-pct {
    min-width: 36px;
    text-align: right;
    color: var(--dim);
    font-size: 0.85em;
  }
  .no-quota { color: var(--dim); font-style: italic; }

  /* ── cooldown / auth badges ── */
  .badge {
    display: inline-block;
    padding: 1px 6px;
    border-radius: 3px;
    font-size: 0.8em;
    font-weight: bold;
  }
  .badge-amber { background: #4b3b0a; color: var(--amber); }
  .badge-grey  { background: #2e3338; color: var(--grey); }

  /* ── log panel ── */
  #logs-panel { flex: 2; min-height: 0; }
  #log-body {
    padding: 6px 10px;
    font-size: 0.9em;
    color: var(--dim);
    overflow-y: auto;
    height: 100%;
  }
  #log-body .line { white-space: pre; line-height: 1.5; }

  /* ── legend bar ── */
  #legend {
    display: flex;
    gap: 16px;
    align-items: center;
    flex-wrap: wrap;
    padding: 4px 6px;
    font-size: 0.82em;
    color: var(--dim);
    border-top: 1px solid var(--border);
    background: var(--bg2);
    flex-shrink: 0;
  }
  #legend .leg-item { display: flex; align-items: center; gap: 5px; }

  /* ── status bar ── */
  #statusbar {
    font-size: 0.8em;
    color: var(--dim);
    text-align: right;
    flex-shrink: 0;
    padding: 0 4px;
  }
  #statusbar .ok   { color: var(--green); }
  #statusbar .err  { color: var(--red); }
</style>
</head>
<body>

<div class="panel" id="providers-panel">
  <div class="panel-header">
    <span>chicco</span>
    <span class="sub" id="header-meta">loading…</span>
  </div>
  <div class="panel-body">
    <table id="providers-table">
      <thead>
        <tr>
          <th style="width:22px"></th>
          <th>provider</th>
          <th>kind</th>
          <th>model</th>
          <th>used / quota</th>
          <th>requests</th>
          <th style="min-width:160px">usage</th>
        </tr>
      </thead>
      <tbody id="providers-body"></tbody>
    </table>
  </div>
  <div id="legend">
    <span class="leg-item"><span class="dot dot-green"></span> ready</span>
    <span class="leg-item"><span class="dot dot-amber"></span> cooldown / limit</span>
    <span class="leg-item"><span class="dot dot-grey"></span> bad key / down</span>
    <span class="leg-item"><span class="dot dot-hollow"></span> checking</span>
  </div>
</div>

<div class="panel" id="logs-panel">
  <div class="panel-header">logs</div>
  <div class="panel-body">
    <div id="log-body"></div>
  </div>
</div>

<div id="statusbar">—</div>

<script>
'use strict';

// ── helpers ──────────────────────────────────────────────────────────────────

function fmtTok(n) {
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

function fmtReset(secs) {
  const t = new Date(Date.now() + secs * 1000);
  const h = t.getHours().toString().padStart(2, '0');
  const m = t.getMinutes().toString().padStart(2, '0');
  if (secs < 12 * 3600) return h + ':' + m;
  return t.toLocaleDateString(undefined, {month:'short', day:'numeric'}) + ' ' + h + ':' + m;
}

function fmtDuration(secs) {
  if (secs < 60) return Math.round(secs) + 's';
  if (secs < 3600) return Math.round(secs / 60) + 'm';
  return (secs / 3600).toFixed(1) + 'h';
}

function barColor(pct) {
  if (pct >= 0.85) return 'red';
  if (pct >= 0.60) return 'amber';
  return 'green';
}

function esc(s) {
  return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

// ── rendering ────────────────────────────────────────────────────────────────

function dotHTML(p) {
  if (p.inactive) return '<span class="dot dot-grey" title="not configured"></span>';
  switch (p.health) {
    case 'ok':
      if (p.cooldown_secs > 0 && p.cooldown_kind !== 'limit')
        return '<span class="dot dot-amber" title="cooldown"></span>';
      return '<span class="dot dot-green" title="ready"></span>';
    case 'auth':  return '<span class="dot dot-grey"  title="auth failed"></span>';
    case 'down':  return '<span class="dot dot-grey"  title="unreachable"></span>';
    default:      return '<span class="dot dot-hollow" title="checking…"></span>';
  }
}

function usageHTML(p, tokens, reqs) {
  // Inactive (no api_key/models — never probed), auth, or down: show a badge
  // instead of a bar.
  if (p.inactive)
    return '<span class="badge badge-grey">not configured — check api_key/models</span>';
  if (p.health === 'auth')
    return '<span class="badge badge-grey">auth failed — check API key</span>';
  if (p.health === 'down')
    return '<span class="badge badge-grey">unreachable</span>';
  if (p.health === 'unknown')
    return '<span class="badge badge-grey">checking…</span>';

  // Cooldown + limit: show reset time.
  if (p.cooldown_secs > 0 && p.cooldown_kind === 'limit')
    return '<span class="badge badge-amber">limit · resets ' + esc(fmtReset(p.cooldown_secs)) + '</span>';

  if (p.quota <= 0)
    return '<span class="no-quota">(no quota)</span>';

  const pct = Math.min(p.quota_is_tokens
    ? tokens / p.quota
    : reqs / p.quota, 1);
  const col = barColor(pct);
  const pctLabel = Math.round(pct * 100) + '%';
  const suffix = p.cooldown_secs > 0
    ? ' &nbsp;<span class="badge badge-amber">cd ' + esc(fmtDuration(p.cooldown_secs)) + '</span>'
    : '';
  return '<div class="bar-wrap">'
    + '<div class="bar-track"><div class="bar-fill ' + col + '" style="width:' + (pct*100).toFixed(1) + '%"></div></div>'
    + '<span class="bar-pct">' + esc(pctLabel) + '</span>'
    + suffix
    + '</div>';
}

function usageLabelHTML(p, tokens, reqs) {
  if (p.quota <= 0)
    return esc(fmtTok(tokens)) + ' / —';
  if (p.quota_is_tokens)
    return esc(fmtTok(tokens)) + ' / ' + esc(fmtTok(p.quota));
  return reqs + ' / ' + p.quota + ' req';
}

function renderProviders(providers) {
  const tbody = document.getElementById('providers-body');
  const rows = [];

  for (const p of providers) {
    const models = p.models && p.models.length > 0 ? p.models : [{name:'—', tokens:0, requests:0}];

    // First row: provider level
    const first = models[0];
    rows.push(
      '<tr>',
      '<td>', dotHTML(p), '</td>',
      '<td><strong>', esc(p.name), '</strong></td>',
      '<td style="color:var(--dim)">', esc(p.kind), '</td>',
      '<td style="color:var(--dim)">', esc(first.name), '</td>',
      '<td>', usageLabelHTML(p, p.used_tokens, p.requests), '</td>',
      '<td style="color:var(--dim)">req&nbsp;', p.requests, '</td>',
      '<td>', usageHTML(p, p.used_tokens, p.requests), '</td>',
      '</tr>'
    );

    // Continuation rows: one per extra model
    for (let i = 1; i < models.length; i++) {
      const m = models[i];
      rows.push(
        '<tr>',
        '<td></td>',
        '<td></td>',
        '<td></td>',
        '<td class="model-cont">', esc(m.name), '</td>',
        '<td style="color:var(--dim)">', usageLabelHTML(p, m.tokens, m.requests), '</td>',
        '<td style="color:var(--dim)">req&nbsp;', m.requests, '</td>',
        '<td>', usageHTML(p, m.tokens, m.requests), '</td>',
        '</tr>'
      );
    }
  }

  tbody.innerHTML = rows.join('');
}

// ── log pane ─────────────────────────────────────────────────────────────────

let lastLogCount = 0;

function renderLogs(lines) {
  const el = document.getElementById('log-body');
  const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;

  if (lines.length !== lastLogCount) {
    el.innerHTML = lines.map(l => '<div class="line">' + esc(l) + '</div>').join('');
    lastLogCount = lines.length;
    if (atBottom) el.scrollTop = el.scrollHeight;
  }
}

// ── poll loop ────────────────────────────────────────────────────────────────

const statusEl = document.getElementById('statusbar');
const metaEl   = document.getElementById('header-meta');

async function poll() {
  try {
    const resp = await fetch('/v1/status');
    if (!resp.ok) throw new Error('HTTP ' + resp.status);
    const data = await resp.json();

    const n = (data.providers || []).length;
    metaEl.textContent = window.location.host + ' · ' + n + ' provider' + (n === 1 ? '' : 's');

    renderProviders(data.providers || []);
    renderLogs(data.logs || []);

    const now = new Date().toLocaleTimeString();
    statusEl.innerHTML = '<span class="ok">✓</span> updated ' + esc(now);
  } catch (e) {
    statusEl.innerHTML = '<span class="err">✗</span> ' + esc(String(e));
  }
}

poll();
setInterval(poll, 1000);
</script>
</body>
</html>`
