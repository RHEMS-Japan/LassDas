package main

// pageTemplates is the whole of the page's markup. Labels go through t (the
// viewer's language), stage names through sn, the runtime's status phrases
// through st; everything the runtime wrote is shown as it is.
const pageTemplates = `
{{define "head"}}<!DOCTYPE html><html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="{{.Refresh}}"><title>{{.Title}}</title>
<style>
:root{color-scheme:light dark;--bg:#f6f6f4;--paper:#ffffff;--paper-2:#f1f1ee;--ink:#0f172a;--ink-2:#334155;--muted:#64748b;--line:#e4e4e0;--line-2:#d4d4cf;--accent:#4f46e5;--accent-ink:#fff;--run:#0a7050;--wait:#96520a;--attn:#c01440;--done:#4f46e5;--stopped:#5a5f6e;--queued:#6b7280;--muted-2:#5a5f6e;--run-soft:#e7f6ef;--wait-soft:#fdf1df;--attn-soft:#fde8ed;--done-soft:#eceaff;--stopped-soft:#eef1f5;--shadow:0 1px 2px rgba(15,23,42,.06),0 8px 24px -16px rgba(15,23,42,.25);--radius:14px;--mono:ui-monospace,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace}
@media (prefers-color-scheme:dark){:root{--bg:#0b0f19;--paper:#121826;--paper-2:#171f2e;--ink:#e6e8ee;--ink-2:#c3c8d4;--muted:#8b93a5;--line:#232b3b;--line-2:#2f3950;--accent:#8b85ff;--accent-ink:#0b0f19;--run:#34d399;--wait:#fbbf24;--attn:#fb7185;--done:#a5b4fc;--stopped:#94a3b8;--queued:#8b93a5;--muted-2:#a3aabb;--run-soft:#0f2a22;--wait-soft:#2d2210;--attn-soft:#331420;--done-soft:#1d1b3d;--stopped-soft:#1a2130;--shadow:0 1px 2px rgba(0,0,0,.4),0 12px 32px -20px rgba(0,0,0,.8)}}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.6 "SF Pro Text",-apple-system,BlinkMacSystemFont,"Hiragino Sans","Hiragino Kaku Gothic ProN","Noto Sans JP",system-ui,sans-serif;font-feature-settings:"tnum";-webkit-font-smoothing:antialiased}
a{color:var(--accent);text-decoration-thickness:1px;text-underline-offset:.15em}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px;border-radius:6px}
.skip{position:absolute;left:-999em;top:.5em;background:var(--accent);color:var(--accent-ink);padding:.4em .8em;border-radius:8px;z-index:6}.skip:focus{left:.5em}
.refresh{position:fixed;top:0;left:0;height:2px;width:0;background:var(--accent);z-index:5;animation:refresh linear forwards;opacity:.8}
@keyframes refresh{from{width:0}to{width:100%}}
.top{position:sticky;top:0;z-index:4;background:var(--paper);background:color-mix(in srgb,var(--paper) 86%,transparent);backdrop-filter:saturate(1.4) blur(12px);-webkit-backdrop-filter:saturate(1.4) blur(12px);border-bottom:1px solid var(--line);padding:.75em 1.6em;display:flex;flex-wrap:wrap;gap:.4em 1.6em;align-items:center}
.top .brand{font-weight:700;letter-spacing:-.01em;display:inline-flex;align-items:center;gap:.55em}
.top .brand::before{content:"";width:.55em;height:.55em;border-radius:50%;background:var(--run);box-shadow:0 0 0 0 var(--run);animation:pulse 2.4s ease-out infinite}
.top nav{display:flex;flex-wrap:wrap;gap:.2em 1.2em}.top nav a{color:var(--muted);text-decoration:none;font-size:.93em;padding:.2em 0;border-bottom:1px solid transparent;transition:color .15s,border-color .15s}.top nav a:hover{color:var(--ink);border-bottom-color:var(--line-2)}
.top .lang{margin-left:auto}.top .lang a{text-decoration:none;font-size:.85em;border:1px solid var(--line-2);border-radius:999px;padding:.2em .85em;color:var(--muted);transition:color .15s,border-color .15s,background .15s}.top .lang a:hover{color:var(--ink);background:var(--paper-2)}
main{max-width:1520px;margin:0 auto;padding:1.6em 1.6em 4em}
h1{font-size:2em;line-height:1.15;letter-spacing:-.025em;margin:.25em 0 .15em;font-weight:750}
h1 .key{font-size:.5em;letter-spacing:.02em;color:var(--muted);text-decoration:none;display:block;margin-bottom:.35em;font-weight:600}
.sub{color:var(--muted-2);font-size:.9em;margin:0 0 1.4em}
h2{font-size:1.05em;margin:0 0 .7em;letter-spacing:-.01em}
h3{font-size:.95em;margin:.8em 0 .3em}
.panel{background:var(--paper);border:1px solid var(--line);border-radius:var(--radius);padding:1.2em 1.4em;margin:1.2em 0;box-shadow:var(--shadow)}
.strip{display:flex;flex-wrap:wrap;gap:.5em;margin:.5em 0 1.1em}.strip .badge{font-size:.92em;padding:.2em .9em}
.summary{display:grid;grid-template-columns:repeat(auto-fit,minmax(9.5em,1fr));gap:.7em;margin:.2em 0 1.6em}
.summary .badge{display:flex;flex-direction:column-reverse;align-items:flex-start;gap:.15em;color:var(--muted);border:1px solid var(--line);border-radius:var(--radius);padding:.9em 1.1em .8em;font-size:.82em;line-height:1.2;box-shadow:var(--shadow);position:relative;overflow:hidden;white-space:nowrap}
.summary .badge{box-shadow:var(--shadow),inset 4px 0 0 var(--stopped)}.summary .badge::before{display:none}
.summary .badge b{font-size:2.1em;line-height:1;color:var(--ink);letter-spacing:-.03em;font-weight:700}
.summary .badge.running{box-shadow:var(--shadow),inset 4px 0 0 var(--run)}.summary .badge.awaiting{box-shadow:var(--shadow),inset 4px 0 0 var(--wait)}.summary .badge.attention{box-shadow:var(--shadow),inset 4px 0 0 var(--attn)}.summary .badge.delivered{box-shadow:var(--shadow),inset 4px 0 0 var(--done)}.summary .badge.unchanged{box-shadow:var(--shadow),inset 4px 0 0 var(--done)}.summary .badge.unmerged{box-shadow:var(--shadow),inset 4px 0 0 var(--done)}.summary .badge.queued{box-shadow:var(--shadow),inset 4px 0 0 var(--queued)}
.badge{display:inline-flex;align-items:center;gap:.4em;font-size:.78em;line-height:1.6;padding:.05em .7em;border-radius:999px;color:var(--ink-2);background:var(--stopped-soft);white-space:nowrap;font-weight:600;letter-spacing:.005em}
.badge::before{content:"";width:.5em;height:.5em;border-radius:50%;background:var(--stopped);flex:none}
.badge.running{background:var(--run-soft);color:var(--run)}.badge.running::before{background:var(--run);animation:pulse 2s ease-out infinite}
.badge.awaiting{background:var(--wait-soft);color:var(--wait)}.badge.awaiting::before{background:var(--wait)}
.badge.attention{background:var(--attn-soft);color:var(--attn)}.badge.attention::before{background:var(--attn)}
.badge.delivered{background:var(--done-soft);color:var(--done)}.badge.delivered::before{background:var(--done)}.badge.unchanged{background:var(--done-soft);color:var(--done)}.badge.unchanged::before{background:var(--done)}.badge.unmerged{background:var(--done-soft);color:var(--done)}.badge.unmerged::before{background:var(--done)}
.badge.stopped{background:var(--stopped-soft);color:var(--stopped)}.badge.queued{background:var(--stopped-soft);color:var(--muted)}.badge.queued::before{background:var(--queued)}
@keyframes pulse{0%{box-shadow:0 0 0 0 color-mix(in srgb,currentColor 45%,transparent)}70%{box-shadow:0 0 0 .55em transparent}100%{box-shadow:0 0 0 0 transparent}}
.board-wrap{overflow-x:auto;overscroll-behavior-x:contain;scroll-snap-type:x proximity;padding:.2em 0 1em;margin:0 -1.6em;-webkit-overflow-scrolling:touch}
.board{display:grid;grid-auto-flow:column;grid-auto-columns:minmax(17em,1fr);gap:.8em;padding:0 1.6em;min-width:max-content;align-items:start}
.col{background:var(--paper-2);border:1px solid var(--line);border-radius:var(--radius);padding:.8em .8em .6em;min-height:12em;scroll-snap-align:start;width:17em}
.done-wrap{margin:.4em 0 0}.col.done{background:var(--done-soft);border-color:transparent;width:auto;min-height:0}.col.done .cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(17em,1fr));gap:.7em}.col.done .card{margin:0}
.col h3{margin:0 0 .7em;font-size:.9em;display:flex;justify-content:space-between;align-items:baseline;gap:.4em;letter-spacing:-.005em}
.col h3 small{display:block;color:var(--muted-2);font-weight:500;font-size:.78em;letter-spacing:.02em;font-family:var(--mono)}
.col .count{background:var(--paper);border:1px solid var(--line);border-radius:999px;padding:0 .6em;font-size:.8em;color:var(--muted);font-weight:600;min-width:1.9em;text-align:center}
.col .empty{color:var(--muted-2);font-size:.85em;padding:1.2em .4em;text-align:center;border:1px dashed var(--line-2);border-radius:10px}
.card{display:flex;flex-wrap:wrap;align-items:center;gap:0 .5em;background:var(--paper);border:1px solid var(--line);border-radius:12px;box-shadow:var(--shadow);padding:.85em .95em .75em;margin:.6em 0;transition:transform .18s ease,box-shadow .18s ease,border-color .18s ease;position:relative}
.card>*{flex:0 0 100%;min-width:0}.card>.key{flex:0 1 auto}.card>.badge{flex:0 0 auto;margin-left:auto}
.card:hover{transform:translateY(-2px);box-shadow:0 2px 4px rgba(15,23,42,.06),0 16px 32px -18px rgba(15,23,42,.35);border-color:var(--line-2)}
.card::before{content:"";position:absolute;left:0;top:.9em;bottom:.9em;width:3px;border-radius:2px;background:var(--lane,var(--stopped))}.card{padding-left:1.15em}.card.running{--lane:var(--run)}.card.awaiting{--lane:var(--wait)}.card.attention{--lane:var(--attn)}.card.delivered{--lane:var(--done)}.card.unchanged{--lane:var(--done)}.card.unmerged{--lane:var(--done)}.card.stopped{--lane:var(--stopped);opacity:.8}.card.queued{--lane:var(--queued);border-style:dashed}
.card .key{font-weight:700;text-decoration:none;font-size:.9em;letter-spacing:.01em;color:var(--ink)}.card .key:hover{color:var(--accent)}
.card .key::after{content:"";position:absolute;inset:0;z-index:0}.card>.title,.card>.line,.card>.attn,.card>.fail,.card>.bar{position:relative;z-index:1}
.card .badge{position:relative;z-index:1}
.card .title{margin:.45em 0 .55em;line-height:1.4;font-size:1.02em;font-weight:600;letter-spacing:-.005em;display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.card .line{font-size:.82em;color:var(--muted-2);margin:.2em 0;overflow-wrap:anywhere}.card .line.model{font-family:var(--mono);font-size:.74em}
.bar{height:5px;background:var(--line);border-radius:3px;margin:.6em 0 .45em;overflow:hidden}.bar i{display:block;height:100%;background:var(--accent);border-radius:3px;transition:width .6s ease}
.card.running .bar i{background:linear-gradient(90deg,var(--run),color-mix(in srgb,var(--run) 60%,#fff));background-size:200% 100%;animation:flow 2.2s linear infinite}.card.awaiting .bar i{background:var(--wait)}.card.attention .bar i{background:var(--attn)}.card.delivered .bar i{background:var(--done)}.card.unchanged .bar i{background:var(--done)}.card.unmerged .bar i{background:var(--done)}.card.stopped .bar i{background:var(--stopped)}
@keyframes flow{from{background-position:200% 0}to{background-position:0 0}}
.attn{color:var(--attn);font-size:.85em;white-space:pre-wrap;margin:.45em 0;background:var(--attn-soft);border-radius:8px;padding:.45em .6em}
.fail{color:var(--wait);font-size:.82em;white-space:pre-wrap;word-break:break-word;margin:.45em 0;background:var(--wait-soft);border-radius:8px;padding:.45em .6em;line-height:1.45}
.card .badge{white-space:normal;line-height:1.5;text-align:left}
.meta{color:var(--muted);font-size:.85em}
.err{color:var(--attn)}
pre{background:var(--paper-2);border:1px solid var(--line);border-radius:10px;padding:.8em .95em;margin:.4em 0;font:.83em/1.5 var(--mono);white-space:pre-wrap;word-break:break-word;max-height:30em;overflow:auto;color:var(--ink-2)}
table{border-collapse:collapse;width:100%;background:var(--paper)}
th,td{border-bottom:1px solid var(--line);padding:.55em .7em;text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:600;font-size:.8em;letter-spacing:.03em;text-transform:uppercase}
.grid{display:grid;grid-template-columns:max-content minmax(0,1fr) max-content minmax(0,1fr);gap:.6em 1.4em;margin:0;align-items:baseline}
.grid dt{color:var(--muted);font-size:.78em;letter-spacing:.03em;font-weight:600;white-space:nowrap}.grid dd{margin:0;font-weight:500;overflow-wrap:anywhere}
.pipe{display:flex;flex-wrap:wrap;gap:.4em;align-items:center}
.chip{background:var(--paper-2);border:1px solid var(--line);border-radius:999px;padding:.2em .85em;font-size:.85em;white-space:nowrap;color:var(--ink-2);font-weight:500}
.chip.passed{background:var(--run-soft);color:var(--run);border-color:transparent}.chip.current{background:var(--accent);color:var(--accent-ink);border-color:transparent;box-shadow:0 0 0 4px color-mix(in srgb,var(--accent) 18%,transparent)}.chip.ahead{color:var(--muted)}
.chip small{opacity:.6;margin-left:.35em;font-family:var(--mono);font-size:.85em}
.pipe .arrow{color:var(--line-2)}
.rec{border:1px solid var(--line);border-radius:12px;padding:.8em 1em;margin:.7em 0;background:var(--paper)}
.rec.runtime{background:var(--paper-2);border-style:dashed}.rec.person{background:var(--wait-soft);border-color:color-mix(in srgb,var(--wait) 40%,var(--line))}.rec.error{border-color:color-mix(in srgb,var(--attn) 50%,var(--line))}
.rec .head{display:flex;flex-wrap:wrap;gap:.3em 1em;align-items:baseline;margin-bottom:.3em}
.rec .head .n{color:var(--muted);font-size:.85em;font-family:var(--mono)}
.launch{border:1px solid var(--line);border-radius:12px;padding:.9em 1.1em .9em 1.5em;margin:.9em 0;background:var(--paper);position:relative;box-shadow:var(--shadow)}
.launch::before{content:"";position:absolute;left:0;top:1.1em;bottom:1.1em;width:4px;border-radius:2px;background:var(--stopped)}
.launch.returned::before{background:var(--run)}.launch.failed::before,.launch.could-not-start::before,.launch.interrupted::before{background:var(--attn)}.launch.answer-from-the-requester{background:var(--wait-soft)}.launch.answer-from-the-requester::before{background:var(--wait)}.launch.note-by-the-runtime{border-style:dashed;background:var(--paper-2);box-shadow:none}
.launch .head{display:flex;flex-wrap:wrap;gap:.3em 1em;align-items:baseline;margin-bottom:.4em}.launch .head .n{color:var(--muted);font-size:.85em;font-family:var(--mono)}.launch .head b{font-size:1.02em}
.badge.returned{background:var(--run-soft);color:var(--run)}.badge.returned::before{background:var(--run)}.badge.failed,.badge.could-not-start,.badge.interrupted{background:var(--attn-soft);color:var(--attn)}.badge.failed::before,.badge.could-not-start::before,.badge.interrupted::before{background:var(--attn)}.badge.answer-from-the-requester{background:var(--wait-soft);color:var(--wait)}.badge.answer-from-the-requester::before{background:var(--wait)}.badge.note-by-the-runtime{background:var(--stopped-soft);color:var(--muted)}
.who{display:inline-block;font-size:.75em;color:var(--muted);border:1px solid var(--line-2);border-radius:5px;padding:0 .45em;margin-left:.35em;letter-spacing:.01em;vertical-align:middle}
.why{margin:.4em 0;color:var(--attn)}.empty{color:var(--muted);font-style:italic;margin:.3em 0}.worker{margin:.5em 0 .5em .2em;padding-left:.8em;border-left:2px solid var(--line)}pre.err{border-color:color-mix(in srgb,var(--attn) 50%,var(--line))}
.live{border:1px solid color-mix(in srgb,var(--run) 50%,var(--line));background:linear-gradient(180deg,var(--run-soft),var(--paper) 12em)}
.live h2{color:var(--run);display:flex;align-items:center;gap:.5em;flex-wrap:wrap}.live h2::before{content:"";width:.6em;height:.6em;border-radius:50%;background:var(--run);animation:pulse 1.6s ease-out infinite;flex:none}
details{margin:.4em 0}details summary{cursor:pointer;color:var(--accent);font-size:.9em;padding:.2em 0}details summary:hover{text-decoration:underline}
.files td a{text-decoration:none}
@media (max-width:700px){.path{display:none}main{padding:1em 1em 3em}.top{padding:.6em 1em;display:grid;grid-template-columns:1fr auto;gap:.3em .8em;align-items:center}.top .brand{grid-column:1}.top .lang{grid-column:2;grid-row:1;margin:0}.top nav{grid-column:1 / -1;flex-wrap:nowrap;overflow-x:auto;white-space:nowrap;gap:1em;padding:0 1.6em .1em 0;-webkit-overflow-scrolling:touch;mask-image:linear-gradient(90deg,#000 88%,transparent);-webkit-mask-image:linear-gradient(90deg,#000 88%,transparent)}.top nav a{flex:none}h1{font-size:1.55em}.board-wrap{margin:0 -1em;scroll-snap-type:x mandatory}.board{padding:0 1em;grid-auto-columns:84vw}.col{width:84vw}.summary{grid-template-columns:repeat(2,1fr)}.grid{grid-template-columns:max-content minmax(0,1fr)}}
@media (prefers-reduced-motion:reduce){*,*::before,*::after{animation:none!important;transition:none!important}}
</style></head><body><a class="skip" href="#main">{{t .Lang "skip to content"}}</a><div class="refresh" style="animation-duration:{{.Refresh}}s" aria-hidden="true"></div>{{end}}

{{define "top"}}<header class="top"><span class="brand">{{t .Lang "ticket engine status"}}</span><nav><a href="/">{{t .Lang "overview"}}</a><a href="/config">{{t .Lang "configuration as read"}}</a><a href="/log">{{t .Lang "runtime log"}}</a><a href="/files/">{{t .Lang "every file of the queue"}}</a></nav><span class="lang">{{if eq .Lang "ja"}}<a href="/lang/en">English</a>{{else}}<a href="/lang/ja">日本語</a>{{end}}</span></header>{{end}}

{{define "foot"}}<p class="meta">{{t .Lang "Rendered"}} {{.Now}} · {{t .Lang "this page reloads by itself (every 10 seconds while a process runs, otherwise every 30) and shows the queue as it is on disk. Read only."}}</p></main></body></html>{{end}}

{{define "card"}}<article class="card {{.Lane}}" data-key="{{.Key}}"><a class="key" href="/jobs/{{.ID}}">{{if .Key}}{{.Key}}{{else}}job {{.ID}}{{end}}</a> <span class="badge {{.Lane}}">{{st .Lang .Status}}</span>
<div class="title">{{.Title}}</div>
{{if .StageCount}}<div class="bar"><i style="width:{{pct .StageIndex .StageCount}}%"></i></div><div class="line">{{st .Lang .Position}}</div>{{else}}{{if .Position}}<div class="line">{{st .Lang .Position}}</div>{{end}}{{end}}
{{if .Model}}<div class="line model">{{.Model}}</div>{{end}}
{{if .Attention}}<div class="attn">{{t .Lang .Attention}}</div>{{end}}
{{if .Failure}}<div class="fail">{{t .Lang "last failure"}}: {{.Failure}}</div>{{end}}
<div class="line">{{t .Lang "elapsed"}} {{.Elapsed}} · {{t .Lang "last change"}} {{ago .Lang .Updated}}{{if .Requester}} · {{.Requester}}{{end}}</div></article>{{end}}

{{define "column"}}<section class="col{{if .Col.Done}} done{{end}}" data-stage="{{.Col.Key}}"><h3><span>{{sn .Lang .Col.Key}}{{if and (ne .Col.Key "done") (ne .Col.Key "other")}}<small>{{.Col.Key}}</small>{{end}}</span><span class="count">{{len .Col.Jobs}}</span></h3>
<div class="cards">{{range .Col.Jobs}}{{template "card" (card $.Page .)}}{{else}}<div class="empty">{{t $.Lang "none"}}</div>{{end}}</div></section>{{end}}

{{define "overview"}}{{template "head" (head .Lang "ticket engine status" 30)}}{{template "top" .}}<main id="main">
<h1>{{t .Lang "Requests"}}</h1>
<p class="sub"><span class="path">{{t .Lang "Queue"}} {{.RunDir}} · </span>{{t .Lang "read at"}} {{.Now}}</p>
{{range .Notes}}<p class="err">{{.}}</p>{{end}}
<div class="strip summary">{{range .Lanes}}<span class="badge {{.Key}}">{{t $.Lang .Title}} <b>{{.Count}}</b></span>{{end}}</div>
{{if not .Jobs}}<p class="meta">{{t .Lang "No request has been accepted into this queue yet."}}</p>{{end}}
<div class="board-wrap"><div class="board">{{range .Columns}}{{if not .Done}}{{template "column" (column $ .)}}{{end}}{{end}}</div></div>
{{range .Columns}}{{if .Done}}<div class="done-wrap">{{template "column" (column $ .)}}</div>{{end}}{{end}}
{{if .Stages}}<div class="panel"><h2>{{t .Lang "Stages of the run"}}</h2><div class="pipe">{{range $i, $s := .Stages}}{{if $i}}<span class="arrow">→</span>{{end}}<span class="chip">{{sn $.Lang $s}}<small>{{$s}}</small></span>{{end}}</div></div>{{end}}
{{if .Config}}<div class="panel"><h2>{{t .Lang "Intake, as configured"}}</h2><pre>{{pretty .Intake}}</pre></div>
<div class="panel"><h2>{{t .Lang "Decision and models"}}</h2><pre>{{pretty .Router}}</pre><pre>{{pretty .Selection}}</pre></div>{{end}}
<div class="panel"><h2>{{t .Lang "Runtime log (tail)"}}</h2>{{if .Log}}<pre>{{.Log}}</pre><p class="meta"><a href="/log">{{t .Lang "the whole log"}}</a></p>{{else}}<p class="meta">{{t .Lang "(nothing yet)"}}</p>{{end}}</div>
{{template "foot" .}}{{end}}

{{define "files"}}{{template "head" (head .Lang (printf "%s: %s" (t .Lang "files") .Path) 30)}}{{template "top" .}}<main id="main">
<h1>{{.RunDir}}{{if ne .Path "."}}/{{.Path}}{{end}}</h1>
<div class="panel files"><table><tr><th>{{t .Lang "Name"}}</th><th>{{t .Lang "Size"}}</th><th>{{t .Lang "Modified"}}</th></tr>
{{if ne .Path "."}}<tr><td><a href="{{.Base}}../">../</a></td><td></td><td></td></tr>{{end}}
{{range .Files}}<tr><td>{{if .Link}}{{.Name}} <span class="meta">{{t $.Lang "(symbolic link; not followed, the page stays inside the queue)"}}</span>{{else}}<a href="{{$.Base}}{{.Href}}{{if .Dir}}/{{end}}">{{.Name}}{{if .Dir}}/{{end}}</a>{{end}}</td><td>{{if not .Dir}}{{.Size}}{{end}}</td><td>{{time .Modified}}</td></tr>{{end}}
</table></div>
{{template "foot" .}}{{end}}

{{define "job"}}{{with .Job}}{{template "head" (head $.Lang (printf "%s %s" .Key (t $.Lang "status")) .Refresh)}}{{template "top" $}}<main id="main">
<h1><a class="key" href="/jobs/{{.ID}}">{{if .Key}}{{.Key}}{{else}}job {{.ID}}{{end}}</a> {{.Title}}</h1>
<div class="strip"><span class="badge {{.Lane}}">{{st $.Lang .Status}}</span>{{if .State}}{{if and .State.Waiting (ne .Lane "awaiting") (not .Stopped)}}<span class="badge awaiting">{{t $.Lang "waiting for the requester"}}</span>{{end}}{{end}}</div>
{{if .Attention}}<p class="attn">{{t $.Lang .Attention}}</p>{{end}}
{{if .Failure}}<p class="fail">{{t $.Lang "last failure"}}: {{.Failure}}</p>{{end}}
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
<div class="panel"><h2>{{t $.Lang "What happened, launch by launch"}} ({{if eq $.Lang "ja"}}{{.LaunchCount}} 回の起動{{else}}{{.LaunchCount}} {{if eq .LaunchCount 1}}launch{{else}}launches{{end}}{{end}}, {{len .Launches}} {{t $.Lang "entries"}})</h2>
{{range .Launches}}{{$launch := .}}<div class="launch {{.Outcome | cls}}"><div class="head"><span class="n">{{if gt .Count 1}}{{t $.Lang "launch"}} {{.Index}}–{{.Last}}{{else}}{{t $.Lang "launch"}} {{.Index}}{{end}}</span><b>{{sn $.Lang .Role}}</b><span class="meta">{{.Role}}</span><span class="badge {{.Outcome | cls}}">{{t $.Lang .Outcome}}{{if gt .Count 1}} · {{t $.Lang "the same failure, repeated"}} {{.Count}} {{t $.Lang "times"}}{{end}}</span><span class="meta">{{if .Started.IsZero}}{{time .Finished}}{{else}}{{time .Started}} → {{time .Finished}}{{end}}{{if .Duration}} ({{.Duration}}){{end}}{{if .Gap}} · {{.Gap}} {{t $.Lang "after the previous record"}}{{end}}</span></div>
{{if .Failure}}<p class="why"><b>{{t $.Lang "why it ended so"}}</b> <span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the runtime"}}</span><br>{{if .RuntimeFailure}}{{t $.Lang .Failure}}{{else}}{{.Failure}}{{end}}</p>{{end}}
{{range .Workers}}<div class="worker">{{if eq $launch.Outcome "answer from the requester"}}<p class="meta"><b>{{t $.Lang "what the requester wrote"}}</b> <span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the requester"}}</span></p>{{else}}<p class="meta"><b>{{t $.Lang "what the worker wrote"}}</b> <span class="who">{{t $.Lang "written by"}}: {{if .Model}}{{t $.Lang "the worker"}} {{.Model}}{{else}}{{t $.Lang "the command"}} {{.Speaker}}{{end}}</span></p>{{end}}{{if .Output}}<pre>{{.Output}}</pre>{{else}}<p class="empty">{{if eq $launch.Outcome "could not start"}}{{t $.Lang "no output: the process could not start"}}{{else}}{{t $.Lang "no output"}}{{end}}</p>{{end}}
{{if .Error}}<p class="meta"><b>{{t $.Lang "the process ended with an error"}}</b> <span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the runtime (how it ended), then the worker's stderr"}}</span></p><pre class="err">{{.Error}}</pre>{{end}}
{{if .Diagnostics}}<details><summary>{{t $.Lang "the worker's own stderr"}} <span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the worker, with the runtime's notes"}}</span></summary><pre>{{.Diagnostics}}</pre></details>{{end}}</div>{{end}}
{{range .Notes}}<p class="meta"><b>{{t $.Lang "what the runtime observed"}}</b> <span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the runtime"}}</span></p><pre>{{t $.Lang .}}</pre>{{end}}
{{if .Instruction}}<details><summary>{{t $.Lang "what the worker was handed"}}</summary><p class="meta"><span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the operator's settings"}}</span> {{t $.Lang "the role's purpose and the operator's instructions"}}</p><p class="meta"><span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the requester"}}</span> {{t $.Lang "your ticket text, as written at the tracker (shown above as the request)"}}</p><p class="meta"><span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the runtime"}}</span> {{t $.Lang "the runtime's own words about this stage and the role"}}</p><pre>{{.Instruction}}</pre><p class="meta"><span class="who">{{t $.Lang "written by"}}: {{t $.Lang "the worker"}}</span> {{t $.Lang "the record up to this launch, collapsed: identical failures as one entry, at most the latest sixty"}}</p></details>{{end}}</div>{{end}}
<details><summary>{{t $.Lang "raw records"}} ({{len .Records}} {{t $.Lang "entries"}})</summary>
{{range .Records}}<div class="rec{{if .Runtime}} runtime{{end}}{{if .Person}} person{{end}}{{if .Error}} error{{end}}"><div class="head"><span class="n">{{.Index}}.</span><b>{{sn $.Lang .Role}}</b><span class="meta">{{.Role}} · {{.Speaker}}{{if .Model}} · {{.Model}}{{end}}</span><span class="meta">{{time .Started}} → {{time .Finished}} ({{.Duration}}){{if .Gap}} · {{.Gap}} {{t $.Lang "after the previous record"}}{{end}}</span></div>
{{if .Error}}<p class="err"><b>{{t $.Lang "Error"}}</b></p><pre>{{if .Runtime}}{{t $.Lang .Error}}{{else}}{{.Error}}{{end}}</pre>{{end}}
<details><summary>{{t $.Lang "instruction"}}</summary><pre>{{.Instruction}}</pre></details>
<p class="meta">{{t $.Lang "Output"}}</p><pre>{{.Output}}</pre>
{{if .Diagnostics}}<details><summary>{{t $.Lang "diagnostics (stderr)"}}</summary><pre>{{.Diagnostics}}</pre></details>{{end}}</div>{{end}}</details></div>
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
