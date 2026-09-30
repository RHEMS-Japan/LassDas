package main

// pageTemplates is the whole of the page's markup. Labels go through t (the
// viewer's language), stage names through sn, the runtime's status phrases
// through st; everything the runtime wrote is shown as it is.
const pageTemplates = `
{{define "head"}}<!DOCTYPE html><html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="{{.Refresh}}"><title>{{.Title}}</title>
<style>
:root{--bg:#f3f4f6;--panel:#fff;--line:#e5e7eb;--text:#1f2937;--muted:#6b7280;--accent:#2563eb;--run:#059669;--wait:#d97706;--attn:#dc2626;--done:#2563eb;--soft:#eef2f7}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI","Hiragino Sans","Hiragino Kaku Gothic ProN","Noto Sans JP",Meiryo,sans-serif}
a{color:var(--accent)}
.top{position:sticky;top:0;z-index:2;background:#fff;border-bottom:1px solid var(--line);padding:.55em 1.4em;display:flex;flex-wrap:wrap;gap:.4em 1.4em;align-items:center}
.top .brand{font-weight:700;letter-spacing:.01em}
.top nav{display:flex;flex-wrap:wrap;gap:.2em 1.1em}.top nav a{color:var(--muted);text-decoration:none;font-size:.95em}.top nav a:hover{color:var(--text)}
.top .lang{margin-left:auto}.top .lang a{text-decoration:none;font-size:.9em;border:1px solid var(--line);border-radius:999px;padding:.15em .7em;color:var(--muted)}
main{max-width:1440px;margin:0 auto;padding:1.2em 1.4em 3em}
h1{font-size:1.35em;margin:.3em 0 .2em}
.sub{color:var(--muted);font-size:.9em;margin:0 0 1em}
h2{font-size:1.05em;margin:0 0 .6em}
h3{font-size:.95em;margin:.8em 0 .3em}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:1em 1.2em;margin:1em 0}
.strip{display:flex;flex-wrap:wrap;gap:.6em;margin:.4em 0 1em}
.badge{display:inline-block;font-size:.8em;line-height:1.7;padding:0 .7em;border-radius:999px;color:#fff;background:var(--muted);white-space:nowrap}
.badge b{font-weight:700}
.badge.running{background:var(--run)}.badge.awaiting{background:var(--wait)}.badge.attention{background:var(--attn)}.badge.delivered{background:var(--done)}
.board{display:grid;grid-template-columns:repeat(auto-fit,minmax(12.5em,1fr));gap:.7em;padding:.2em 0 .8em}
.col{background:#e9ecf1;border-radius:10px;padding:.55em .6em;min-height:9em}
.col.done{background:#e3ebfa}
.col h3{margin:0 0 .5em;font-size:.9em;display:flex;justify-content:space-between;align-items:baseline;gap:.4em}
.col h3 small{display:block;color:var(--muted);font-weight:400;font-size:.8em}
.col .count{background:#fff;border-radius:999px;padding:0 .55em;font-size:.8em;color:var(--muted)}
.col .empty{color:#9ca3af;font-size:.85em;padding:.3em .2em}
.card{background:#fff;border-radius:8px;box-shadow:0 1px 2px rgba(0,0,0,.12);padding:.7em .8em .6em;margin:.5em 0;border-left:4px solid var(--muted)}
.card.running{border-color:var(--run)}.card.awaiting{border-color:var(--wait)}.card.attention{border-color:var(--attn)}.card.delivered{border-color:var(--done)}
.card .key{font-weight:700;text-decoration:none}
.card .title{margin:.25em 0 .5em;line-height:1.35;display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.card .line{font-size:.85em;color:var(--muted);margin:.2em 0}
.bar{height:4px;background:#e5e7eb;border-radius:2px;margin:.5em 0 .4em}.bar i{display:block;height:100%;background:var(--accent);border-radius:2px}
.attn{color:var(--attn);font-size:.85em;white-space:pre-wrap;margin:.3em 0}
.fail{color:var(--wait);font-size:.85em;white-space:pre-wrap;word-break:break-word;margin:.3em 0}
.card .badge{white-space:normal;line-height:1.5;padding:.1em .7em}
.meta{color:var(--muted);font-size:.85em}
.err{color:var(--attn)}
pre{background:#f8f9fb;border:1px solid var(--line);border-radius:6px;padding:.7em .8em;margin:.4em 0;font-size:.85em;line-height:1.45;white-space:pre-wrap;word-break:break-word;max-height:30em;overflow:auto}
table{border-collapse:collapse;width:100%;background:#fff}
th,td{border-bottom:1px solid var(--line);padding:.45em .6em;text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:600;font-size:.85em}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(11em,1fr));gap:.5em 1.4em;margin:0}
.grid dt{color:var(--muted);font-size:.8em}.grid dd{margin:0 0 .3em}
.pipe{display:flex;flex-wrap:wrap;gap:.35em;align-items:center}
.chip{background:var(--soft);border-radius:999px;padding:.12em .75em;font-size:.85em;white-space:nowrap}
.chip.passed{background:#d1fae5;color:#065f46}.chip.current{background:var(--accent);color:#fff}.chip.ahead{color:var(--muted)}
.chip small{opacity:.65;margin-left:.3em}
.pipe .arrow{color:#9ca3af}
.rec{border:1px solid var(--line);border-radius:8px;padding:.7em .9em;margin:.6em 0;background:#fff}
.rec.runtime{background:#f8f9fb;border-style:dashed}.rec.person{background:#fff8e6;border-color:#f3d38a}.rec.error{border-color:#fca5a5}
.rec .head{display:flex;flex-wrap:wrap;gap:.3em 1em;align-items:baseline;margin-bottom:.3em}
.rec .head .n{color:var(--muted);font-size:.85em}
.live{border:2px solid var(--run);background:#f0fdf4}
.live h2{color:#065f46}
details{margin:.3em 0}details summary{cursor:pointer;color:var(--accent);font-size:.9em}
.files td a{text-decoration:none}
@media (max-width:700px){main{padding:1em .8em}.top{padding:.5em .8em}}
</style></head><body>{{end}}

{{define "top"}}<header class="top"><span class="brand">{{t .Lang "ticket engine status"}}</span><nav><a href="/">{{t .Lang "overview"}}</a><a href="/config">{{t .Lang "configuration as read"}}</a><a href="/log">{{t .Lang "runtime log"}}</a><a href="/files/">{{t .Lang "every file of the queue"}}</a></nav><span class="lang">{{if eq .Lang "ja"}}<a href="/lang/en">English</a>{{else}}<a href="/lang/ja">日本語</a>{{end}}</span></header>{{end}}

{{define "foot"}}<p class="meta">{{t .Lang "Rendered"}} {{.Now}} · {{t .Lang "this page reloads by itself (every 10 seconds while a process runs, otherwise every 30) and shows the queue as it is on disk. Read only."}}</p></main></body></html>{{end}}

{{define "card"}}<article class="card {{.Lane}}" data-key="{{.Key}}"><a class="key" href="/jobs/{{.ID}}">{{if .Key}}{{.Key}}{{else}}job {{.ID}}{{end}}</a> <span class="badge {{.Lane}}">{{st .Lang .Status}}</span>
<div class="title">{{.Title}}</div>
{{if .StageCount}}<div class="bar"><i style="width:{{pct .StageIndex .StageCount}}%"></i></div><div class="line">{{st .Lang .Position}}</div>{{else}}{{if .Position}}<div class="line">{{st .Lang .Position}}</div>{{end}}{{end}}
{{if .Model}}<div class="line">{{.Model}}</div>{{end}}
{{if .Attention}}<div class="attn">{{t .Lang .Attention}}</div>{{end}}
{{if .Failure}}<div class="fail">{{t .Lang "last failure"}}: {{.Failure}}</div>{{end}}
<div class="line">{{t .Lang "elapsed"}} {{.Elapsed}} · {{t .Lang "last change"}} {{ago .Lang .Updated}}{{if .Requester}} · {{.Requester}}{{end}}</div></article>{{end}}

{{define "overview"}}{{template "head" (head .Lang "ticket engine status" 30)}}{{template "top" .}}<main>
<h1>{{t .Lang "Requests"}}</h1>
<p class="sub">{{t .Lang "Queue"}} {{.RunDir}} · {{t .Lang "read at"}} {{.Now}}</p>
{{range .Notes}}<p class="err">{{.}}</p>{{end}}
<div class="strip">{{range .Lanes}}<span class="badge {{.Key}}">{{t $.Lang .Title}} <b>{{.Count}}</b></span>{{end}}</div>
{{if not .Jobs}}<p class="meta">{{t .Lang "No request has been accepted into this queue yet."}}</p>{{end}}
<div class="board">{{range .Columns}}<section class="col{{if .Done}} done{{end}}" data-stage="{{.Key}}"><h3><span>{{sn $.Lang .Key}}{{if and (ne .Key "done") (ne .Key "other")}}<small>{{.Key}}</small>{{end}}</span><span class="count">{{len .Jobs}}</span></h3>
{{range .Jobs}}{{template "card" (card $ .)}}{{else}}<div class="empty">{{t $.Lang "none"}}</div>{{end}}</section>{{end}}</div>
{{if .Stages}}<div class="panel"><h2>{{t .Lang "Stages of the run"}}</h2><div class="pipe">{{range $i, $s := .Stages}}{{if $i}}<span class="arrow">→</span>{{end}}<span class="chip">{{sn $.Lang $s}}<small>{{$s}}</small></span>{{end}}</div></div>{{end}}
{{if .Config}}<div class="panel"><h2>{{t .Lang "Intake, as configured"}}</h2><pre>{{pretty .Intake}}</pre></div>
<div class="panel"><h2>{{t .Lang "Decision and models"}}</h2><pre>{{pretty .Router}}</pre><pre>{{pretty .Selection}}</pre></div>{{end}}
<div class="panel"><h2>{{t .Lang "Runtime log (tail)"}}</h2>{{if .Log}}<pre>{{.Log}}</pre><p class="meta"><a href="/log">{{t .Lang "the whole log"}}</a></p>{{else}}<p class="meta">{{t .Lang "(nothing yet)"}}</p>{{end}}</div>
{{template "foot" .}}{{end}}

{{define "files"}}{{template "head" (head .Lang (printf "%s: %s" (t .Lang "files") .Path) 30)}}{{template "top" .}}<main>
<h1>{{.RunDir}}{{if ne .Path "."}}/{{.Path}}{{end}}</h1>
<div class="panel files"><table><tr><th>{{t .Lang "Name"}}</th><th>{{t .Lang "Size"}}</th><th>{{t .Lang "Modified"}}</th></tr>
{{if ne .Path "."}}<tr><td><a href="{{.Base}}../">../</a></td><td></td><td></td></tr>{{end}}
{{range .Files}}<tr><td>{{if .Link}}{{.Name}} <span class="meta">{{t $.Lang "(symbolic link; not followed, the page stays inside the queue)"}}</span>{{else}}<a href="{{$.Base}}{{.Href}}{{if .Dir}}/{{end}}">{{.Name}}{{if .Dir}}/{{end}}</a>{{end}}</td><td>{{if not .Dir}}{{.Size}}{{end}}</td><td>{{time .Modified}}</td></tr>{{end}}
</table></div>
{{template "foot" .}}{{end}}

{{define "job"}}{{with .Job}}{{template "head" (head $.Lang (printf "%s %s" .Key (t $.Lang "status")) .Refresh)}}{{template "top" $}}<main>
<h1><a class="key" href="/jobs/{{.ID}}">{{if .Key}}{{.Key}}{{else}}job {{.ID}}{{end}}</a> {{.Title}}</h1>
<div class="strip"><span class="badge {{.Lane}}">{{st $.Lang .Status}}</span>{{if .State}}{{if .State.Recovering}}<span class="badge">{{t $.Lang "recovering"}}</span>{{end}}{{if and .State.Waiting (ne .Lane "awaiting")}}<span class="badge awaiting">{{t $.Lang "waiting for the requester"}}</span>{{end}}{{end}}</div>
{{range .Notes}}<p class="err">{{.}}</p>{{end}}
<div class="panel"><dl class="grid">
<dt>{{t $.Lang "Position"}}</dt><dd>{{if .Position}}{{st $.Lang .Position}}{{else}}—{{end}}</dd>
<dt>{{t $.Lang "elapsed"}}</dt><dd>{{.Elapsed}}</dd>
<dt>{{t $.Lang "started"}}</dt><dd>{{time .Started}}</dd>
<dt>{{t $.Lang "last change"}}</dt><dd>{{time .Updated}} ({{ago $.Lang .Updated}})</dd>
<dt>{{t $.Lang "Requested by"}}</dt><dd>{{.Requester}} · {{.Created}}</dd>
{{if .Model}}<dt>{{t $.Lang "model"}}</dt><dd>{{.Model}}</dd>{{end}}
{{if .State}}{{if .State.Pending}}<dt>{{t $.Lang "pending:"}}</dt><dd>{{sn $.Lang .State.Pending.Role}}</dd>{{end}}{{end}}
<dt>{{t $.Lang "links"}}</dt><dd>{{if .Link}}<a href="{{.Link}}">{{t $.Lang "open the issue"}}</a> · {{end}}<a href="/files/jobs/{{.ID}}/">{{t $.Lang "every file of this request"}}</a> · <a href="/jobs/{{.ID}}/workspace">{{t $.Lang "workspace changes as text"}}</a></dd>
</dl>
{{if .Trail}}<div class="pipe" style="margin-top:.8em">{{range $i, $s := .Trail}}{{if $i}}<span class="arrow">→</span>{{end}}<span class="chip {{$s.State}}">{{sn $.Lang $s.Name}}<small>{{$s.Name}}</small></span>{{end}}</div>{{end}}
<p class="meta" style="margin:.8em 0 0">{{t $.Lang "Raw files:"}} <a href="/jobs/{{.ID}}/raw/issue.json">issue.json</a> · <a href="/jobs/{{.ID}}/raw/request.txt">request.txt</a> · <a href="/jobs/{{.ID}}/raw/history.json">history.json</a> · <a href="/jobs/{{.ID}}/raw/engine.json">engine.json</a> · <a href="/jobs/{{.ID}}/raw/notices.json">notices.json</a></p>
{{if .State}}{{if .State.Pending}}{{if .State.Pending.Instruction}}<details><summary>{{t $.Lang "instruction of the pending action"}} ({{sn $.Lang .State.Pending.Role}})</summary><pre>{{.State.Pending.Instruction}}</pre></details>{{end}}{{end}}{{end}}
{{if .State}}{{if .State.Workflow}}<details><summary>{{t $.Lang "workflow as recorded for this request"}}</summary><pre>{{pretty .State.Workflow}}</pre></details>{{end}}{{end}}</div>
{{range .Live}}<div class="panel live"><h2>{{t $.Lang "Running now:"}} {{sn $.Lang .Role}} <small class="meta">{{.Role}} · {{.Speaker}}{{if .Model}} · {{.Model}}{{end}}</small></h2>
<p class="meta">{{t $.Lang "since"}} {{time .Started}} · {{ago $.Lang .Started}}</p>
<details><summary>{{t $.Lang "instruction handed to it"}}</summary><pre>{{.Instruction}}</pre></details>
<h3>{{t $.Lang "output so far"}}</h3><pre>{{.Stdout}}</pre>{{if .Stderr}}<h3>{{t $.Lang "diagnostics so far"}}</h3><pre>{{.Stderr}}</pre>{{end}}
{{if .AgentLog}}<h3>{{t $.Lang "the native agent's own log so far"}}{{if .Home}} (<a href="/files/{{.Home}}/logs/agent.log">{{t $.Lang "whole file"}}</a>){{end}}</h3><pre>{{.AgentLog}}</pre>{{end}}</div>{{end}}
{{if .Stages}}<div class="panel"><h2>{{t $.Lang "Time by stage"}}</h2><table><tr><th>{{t $.Lang "Stage"}}</th><th>{{t $.Lang "Launches"}}</th><th>{{t $.Lang "Failed"}}</th><th>{{t $.Lang "Total time (sum over process runs; parallel processes add up)"}}</th><th>{{t $.Lang "First started"}}</th><th>{{t $.Lang "Last finished"}}</th></tr>
{{range .Stages}}<tr><td>{{sn $.Lang .Name}} <small class="meta">{{.Name}}</small></td><td>{{.Launches}}</td><td>{{.Failures}}</td><td>{{.Total}}</td><td>{{time .First}}</td><td>{{time .Last}}</td></tr>{{end}}</table></div>{{end}}
<div class="panel"><h2>{{t $.Lang "Request"}}</h2><pre>{{.Request}}</pre></div>
{{if .Notices}}<div class="panel"><h2>{{t $.Lang "Notices posted by the runtime"}}</h2>{{range .Notices}}<pre>{{pretty .}}</pre>{{end}}</div>{{end}}
<div class="panel"><h2>{{t $.Lang "Record"}} ({{len .Records}} {{t $.Lang "entries"}})</h2>
{{range .Records}}<div class="rec{{if .Runtime}} runtime{{end}}{{if .Person}} person{{end}}{{if .Error}} error{{end}}"><div class="head"><span class="n">{{.Index}}.</span><b>{{sn $.Lang .Role}}</b><span class="meta">{{.Role}} · {{.Speaker}}{{if .Model}} · {{.Model}}{{end}}</span><span class="meta">{{time .Started}} → {{time .Finished}} ({{.Duration}}){{if .Gap}} · {{.Gap}} {{t $.Lang "after the previous record"}}{{end}}</span></div>
{{if .Error}}<p class="err"><b>{{t $.Lang "Error"}}</b></p><pre>{{if .Runtime}}{{t $.Lang .Error}}{{else}}{{.Error}}{{end}}</pre>{{end}}
<details><summary>{{t $.Lang "instruction"}}</summary><pre>{{.Instruction}}</pre></details>
<p class="meta">{{t $.Lang "Output"}}</p><pre>{{.Output}}</pre>
{{if .Diagnostics}}<details><summary>{{t $.Lang "diagnostics (stderr)"}}</summary><pre>{{.Diagnostics}}</pre></details>{{end}}</div>{{end}}</div>
{{if .Answers}}<div class="panel"><h2>{{t $.Lang "Requester answers consumed"}}</h2>{{range .Answers}}<p class="meta">{{.Name}}</p><pre>{{.Text}}</pre>{{end}}</div>{{end}}
{{if .Reviews}}<div class="panel"><h2>{{t $.Lang "Review findings kept by the review command"}}</h2>{{range .Reviews}}<p class="meta">{{.Name}}</p><pre>{{.Text}}</pre>{{end}}</div>{{end}}
{{if .Report}}<div class="panel"><h2>{{t $.Lang "Report written in the workspace"}}</h2><pre>{{.Report}}</pre></div>{{end}}
{{if .Receipt}}<div class="panel"><h2>{{t $.Lang "Delivery receipt written by the delivery program"}}</h2><pre>{{.Receipt}}</pre></div>{{end}}
{{if .Homes}}<div class="panel"><h2>{{t $.Lang "What each role's native agent logged in its own directory"}}</h2>{{range .Homes}}{{$h := .}}<div class="rec"><div class="head"><b>{{t $.Lang "home"}} {{$h.Name}}</b><span class="meta">{{t $.Lang "files:"}} {{range $i, $f := $h.Files}}{{if $i}}, {{end}}{{if $f.Link}}{{$f.Name}} <span class="meta">{{t $.Lang "(symbolic link; not followed, the page stays inside the queue)"}}</span>{{else}}<a href="/files/jobs/{{$.Job.ID}}/homes/{{$h.Name}}/{{$f.Name}}">{{$f.Name}}</a>{{end}}{{end}}{{if $h.MoreFiles}} <span class="meta">(+{{$h.MoreFiles}} {{t $.Lang "more under files"}}: <a href="/files/jobs/{{$.Job.ID}}/homes/{{$h.Name}}/">{{$h.Name}}/</a>)</span>{{end}}</span></div>
{{if $h.Calls}}<p class="meta">{{t $.Lang "as the agent logged it:"}} {{$h.Calls}} {{t $.Lang "model calls"}}, {{$h.TokensIn}} {{t $.Lang "input tokens"}}, {{$h.TokensOut}} {{t $.Lang "output tokens"}}</p>{{end}}{{if $h.UsageNote}}<p class="meta">{{$h.UsageNote}}</p>{{end}}
{{if $h.AgentLog}}<details><summary>{{t $.Lang "agent.log (tail)"}}</summary><pre>{{$h.AgentLog}}</pre></details>{{end}}{{if $h.ErrorsLog}}<details><summary>{{t $.Lang "errors.log (tail)"}}</summary><pre>{{$h.ErrorsLog}}</pre></details>{{end}}
{{if $h.Transcript}}<details><summary>{{t $.Lang "the whole conversation the agent had (messages, tool calls and their results)"}}</summary><pre>{{$h.Transcript}}</pre></details>{{end}}</div>{{end}}</div>{{end}}
<div class="panel"><h2>{{t $.Lang "Workspace changes"}}</h2>{{with .Workspace}}{{if .Note}}<p class="meta">{{t $.Lang .Note}}</p>{{end}}
{{if .Status}}<pre>{{.Status}}</pre>{{else}}{{if not .Note}}<p class="meta">{{t $.Lang "no change in the checkout"}}</p>{{end}}{{end}}
{{if .Log}}<details><summary>{{t $.Lang "recent commits in the checkout"}}</summary><pre>{{.Log}}</pre></details>{{end}}
{{if .Diff}}<details open><summary>{{t $.Lang "diff of tracked files"}}</summary><pre>{{.Diff}}</pre></details>{{end}}
{{range .Untracked}}<details><summary>{{t $.Lang "new file:"}} {{.Name}}</summary><pre>{{.Text}}</pre></details>{{end}}{{if .NotShown}}<p class="meta">{{.NotShown}} {{t $.Lang "more new files are not shown here; they are under files"}}</p>{{end}}{{end}}</div>
{{template "foot" $}}{{end}}{{end}}
`
