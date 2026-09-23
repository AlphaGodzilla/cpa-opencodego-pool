package mgmtui

import (
	"bytes"
	"html/template"
)

// pageTemplate is intentionally dependency-free: no external scripts, no
// network fetches, no mutation endpoints. The host serves this over the
// plugin resource path, which is not management-authenticated, so it must stay
// strictly read-only.
var pageTemplate = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OpenCode Pool</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 13px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
         margin: 0; padding: 20px; background: Canvas; color: CanvasText; }
  h1 { font-size: 17px; margin: 0 0 4px; }
  h2 { font-size: 13px; margin: 24px 0 8px; text-transform: uppercase;
       letter-spacing: .06em; opacity: .65; }
  .meta { display: flex; flex-wrap: wrap; gap: 6px 18px; opacity: .8; margin-bottom: 12px; }
  .meta code { opacity: .85; }
  .banner { padding: 9px 12px; border-radius: 6px; margin: 10px 0; }
  .banner.ok { background: color-mix(in srgb, Green 14%, transparent); }
  .banner.bad { background: color-mix(in srgb, Red 16%, transparent); }
  table { border-collapse: collapse; width: 100%; margin-top: 6px; }
  th, td { text-align: left; padding: 6px 9px; border-bottom: 1px solid color-mix(in srgb, CanvasText 14%, transparent);
           vertical-align: top; white-space: nowrap; }
  th { font-weight: 600; opacity: .7; font-size: 12px; }
  code, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; }
  .pill { display: inline-block; padding: 1px 7px; border-radius: 999px; font-size: 11px;
          background: color-mix(in srgb, CanvasText 12%, transparent); }
  .pill.ok { background: color-mix(in srgb, Green 22%, transparent); }
  .pill.bad { background: color-mix(in srgb, Red 22%, transparent); }
  .pill.warn { background: color-mix(in srgb, Orange 26%, transparent); }
  .num { text-align: right; font-variant-numeric: tabular-nums; }
  .bar { position: relative; width: 78px; height: 6px; border-radius: 3px;
         background: color-mix(in srgb, CanvasText 16%, transparent); overflow: hidden; }
  .bar > i { position: absolute; inset: 0 auto 0 0; background: currentColor; }
  .muted { opacity: .55; }
  .stats { display: grid; grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); gap: 8px 18px; }
  .stats div { display: flex; justify-content: space-between; gap: 10px;
               border-bottom: 1px dotted color-mix(in srgb, CanvasText 15%, transparent); padding-bottom: 3px; }
  .scroll { overflow-x: auto; }
</style>
</head>
<body>
<h1>OpenCode Pool <span class="muted mono">{{.PluginID}} v{{.Version}}</span></h1>
<div class="meta">
  <span>pool prefix <code>{{.BaseURLPrefix}}</code></span>
  <span>poll <code>{{.PollInterval}}</code></span>
  <span>stale after <code>{{.StaleAfter}}</code></span>
  <span>session ttl <code>{{.SessionTTL}}</code></span>
  <span>sessions <code>{{.Sessions}}</code>/<code>{{.MaxSessions}}</code></span>
  <span>snapshot <code>{{.GeneratedAt}}</code></span>
</div>

{{if .Healthy}}
<div class="banner ok">Routing is active: this plugin answers scheduler.pick for requests that carry a session header.</div>
{{else}}
<div class="banner bad">
  <strong>Routing is disabled.</strong> scheduler.pick is declined and the host's built-in scheduler takes over.
  <ul>{{range .Problems}}<li>{{.}}</li>{{end}}</ul>
</div>
{{end}}

<h2>Credentials</h2>
<div class="scroll">
<table>
  <thead><tr>
    <th>token</th><th>provider</th><th>rolling</th><th>weekly</th><th>monthly</th>
    <th class="num">sessions</th><th>state</th><th>fetched</th><th>last seen</th>
  </tr></thead>
  <tbody>
  {{range .Keys}}
    <tr>
      <td class="mono">{{.Token}}</td>
      <td>{{if .Name}}{{.Name}}{{else}}<span class="muted">-</span>{{end}}<br><span class="muted mono">{{.BaseURL}}</span></td>
      <td>{{template "window" .Rolling}}</td>
      <td>{{template "window" .Weekly}}</td>
      <td>{{template "window" .Monthly}}</td>
      <td class="num">{{.Sessions}}</td>
      <td>
        {{if .Usable}}<span class="pill ok">usable</span>{{else}}<span class="pill bad">excluded</span>{{end}}
        {{if .Exclusion}}<br><span class="muted">{{.Exclusion}}</span>{{end}}
        {{if .Revoked}}<br><span class="pill bad">revoked</span>{{end}}
        {{if .Suspected429}}<br><span class="pill warn">recent 429</span>{{end}}
        {{if gt .FailStreak 0}}<br><span class="muted">fetch failures: {{.FailStreak}}</span>{{end}}
      </td>
      <td class="mono muted">{{if .FetchedAt}}{{.FetchedAt}}{{else}}never{{end}}</td>
      <td class="mono muted">{{if .LastSeenAt}}{{.LastSeenAt}}{{else}}never{{end}}</td>
    </tr>
  {{else}}
    <tr><td colspan="9" class="muted">No credentials tracked yet. Check config_path and base_url_prefix.</td></tr>
  {{end}}
  </tbody>
</table>
</div>

<h2>Counters</h2>
<div class="stats">
  <div><span>picks</span><span class="num">{{.Stats.Picks}}</span></div>
  <div><span>sticky hit</span><span class="num">{{.Stats.Bound}}</span></div>
  <div><span>new assignment</span><span class="num">{{.Stats.Assigned}}</span></div>
  <div><span>rebound</span><span class="num">{{.Stats.Rebound}}</span></div>
  <div><span>pinned</span><span class="num">{{.Stats.Pinned}}</span></div>
  <div><span>all-exhausted fallback</span><span class="num">{{.Stats.Fallback}}</span></div>
  <div><span>declined</span><span class="num">{{.Stats.Unhandled}}</span></div>
  <div><span>mapping failed</span><span class="num">{{.Stats.MappingFailed}}</span></div>
  <div><span>usage fetches</span><span class="num">{{.Stats.Fetches}}</span></div>
  <div><span>fetch failures</span><span class="num">{{.Stats.FetchFailures}}</span></div>
  <div><span>fetch timeouts</span><span class="num">{{.Stats.FetchTimeouts}}</span></div>
  <div><span>revoked credentials</span><span class="num">{{.Stats.Revoked}}</span></div>
  <div><span>429 fast path</span><span class="num">{{.Stats.Suspected429}}</span></div>
  <div><span>config syncs</span><span class="num">{{.Stats.ConfigSyncs}}</span></div>
  <div><span>config sync errors</span><span class="num">{{.Stats.ConfigSyncErrors}}</span></div>
</div>
</body>
</html>
{{define "window"}}{{if and . .HasPercent}}<span class="mono">{{printf "%.0f%%" .Percent}}</span> <span class="bar"><i style="width:{{printf "%.0f" .Percent}}%"></i></span>{{if .ResetsAt}}<br><span class="muted mono">reset {{.ResetsAt}}</span>{{end}}{{if ne .Status "ok"}}<br><span class="pill warn">{{.Status}}</span>{{end}}{{else}}<span class="muted">-</span>{{end}}{{end}}
`))

// Page renders the read-only HTML view.
func Page(snapshot Snapshot) []byte {
	var buffer bytes.Buffer
	if errExecute := pageTemplate.Execute(&buffer, snapshot); errExecute != nil {
		return []byte("<!doctype html><title>OpenCode Pool</title><p>failed to render status page")
	}
	return buffer.Bytes()
}
