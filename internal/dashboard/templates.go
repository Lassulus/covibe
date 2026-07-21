package dashboard

import "html/template"

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>covibe — sessions</title>
<style nonce="{{.Nonce}}">
  :root { color-scheme: dark light; --bg:#0e1116; --card:#171b22; --muted:#8b949e; --fg:#e6edf3; --accent:#4ea1ff; --live:#3fb950; --ended:#8b949e; --start:#d29922; }
  * { box-sizing: border-box; }
  body { margin:0; font:15px/1.5 system-ui,sans-serif; background:var(--bg); color:var(--fg); }
  header { display:flex; align-items:center; justify-content:space-between; gap:1rem; padding:1rem 1.5rem; border-bottom:1px solid #222; }
  header h1 { font-size:1.1rem; margin:0; letter-spacing:.02em; }
  header .who { color:var(--muted); font-size:.85rem; }
  header a { color:var(--accent); text-decoration:none; }
  main { padding:1.5rem; }
  .grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(320px,1fr)); gap:1.25rem; }
  .card { background:var(--card); border:1px solid #222; border-radius:12px; padding:1.1rem; display:flex; flex-direction:column; gap:.6rem; }
  .card h2 { margin:0; font-size:1.05rem; display:flex; align-items:center; gap:.5rem; }
  .dot { width:.6rem; height:.6rem; border-radius:50%; display:inline-block; }
  .dot.live{background:var(--live)} .dot.starting{background:var(--start)} .dot.ended{background:var(--ended)}
  .meta { color:var(--muted); font-size:.82rem; word-break:break-all; }
  .badge { font-size:.7rem; padding:.1rem .45rem; border-radius:999px; border:1px solid #333; color:var(--muted); }
  .qr { align-self:center; background:#fff; padding:8px; border-radius:8px; }
  .qr img { display:block; width:200px; height:200px; image-rendering:pixelated; }
  .link { display:flex; gap:.4rem; }
  .link input { flex:1; background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); border-radius:6px; padding:.4rem .5rem; font-family:ui-monospace,monospace; font-size:.78rem; }
  button { background:#21262d; color:var(--fg); border:1px solid #30363d; border-radius:6px; padding:.4rem .6rem; cursor:pointer; font-size:.8rem; }
  button:hover { border-color:var(--accent); }
  a.open { color:var(--accent); text-decoration:none; font-size:.85rem; }
  .empty { color:var(--muted); text-align:center; padding:3rem; }
  .waiting { color:var(--start); font-size:.82rem; }
  .newform { display:flex; gap:.5rem; flex-wrap:wrap; align-items:center; margin-bottom:1.25rem; }
  .newform input { background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); border-radius:6px; padding:.5rem .6rem; font-size:.85rem; }
  .newform input#newname { min-width:220px; }
  .newform input#newdir { min-width:160px; }
  .newerr { color:#f85149; font-size:.8rem; }
  .actions { margin-top:.2rem; }
  .actions .pane { font-size:.78rem; }
  .modal { position:fixed; inset:0; background:rgba(0,0,0,.6); display:flex; align-items:center; justify-content:center; padding:1.5rem; z-index:10; }
  .modalbox { background:var(--card); border:1px solid #30363d; border-radius:10px; width:min(900px,100%); max-height:85vh; display:flex; flex-direction:column; }
  .modalhead { display:flex; justify-content:space-between; align-items:center; padding:.7rem 1rem; border-bottom:1px solid #30363d; }
  .pane-out { margin:0; padding:1rem; overflow:auto; font-family:ui-monospace,monospace; font-size:.78rem; white-space:pre-wrap; word-break:break-word; }
</style>
</head>
<body>
<header>
  <h1>covibe · co-vibing sessions</h1>
  <div>
    {{if .Relay}}<span class="badge">relay {{.Relay}}</span>{{end}}
    <span class="who">{{if .User.Email}}{{.User.Email}}{{else}}{{.User.Sub}}{{end}}</span>
    <a href="/logout">sign out</a>
  </div>
</header>
<main>
  {{if .CanCreate}}
  <form id="newform" class="newform">
    <input id="newname" name="name" placeholder="new session name" autocomplete="off"
           pattern="[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}" title="letters, digits, space . _ - (no leading . or -)" required>
    <input id="newdir" name="dir" placeholder="subdir (optional)" autocomplete="off">
    <button type="submit">start session</button>
    <span id="newerr" class="newerr"></span>
  </form>
  {{end}}
  <div id="grid" class="grid"></div>
  <div id="empty" class="empty" hidden>No live sessions.{{if not .CanCreate}} Start one with <code>covibe start &lt;name&gt;</code>.{{end}}</div>
</main>
<div id="panemodal" class="modal" hidden>
  <div class="modalbox">
    <div class="modalhead"><strong id="panetitle"></strong><button id="paneclose">close</button></div>
    <pre id="panepre" class="pane-out"></pre>
  </div>
</div>
<script nonce="{{.Nonce}}">
const grid = document.getElementById('grid');
const empty = document.getElementById('empty');
document.getElementById('paneclose').onclick = ()=>{ document.getElementById('panemodal').hidden = true; };

function esc(s){ return String(s??'').replace(/[&<>"']/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }

function card(s){
  const el = document.createElement('div');
  el.className = 'card';
  const started = new Date(s.startedAt).toLocaleTimeString();
  let body = '';
  body += '<h2><span class="dot '+esc(s.status)+'"></span>'+esc(s.name||s.id)+
          (s.viewOnly?' <span class="badge">view-only</span>':'')+'</h2>';
  body += '<div class="meta">'+esc(s.dir||'')+'</div>';
  body += '<div class="meta">'+(s.mux?esc(s.mux)+(s.muxSession?' · '+esc(s.muxSession):''):'')+
          ' · since '+esc(started)+'</div>';
  if (s.browserUrl){
    body += '<div class="qr"><img alt="join QR" src="'+esc(s.qr)+'"></div>';
    body += '<div class="link"><input readonly value="'+esc(s.browserUrl)+'">'+
            '<button data-copy="'+esc(s.browserUrl)+'">copy</button></div>';
    body += '<a class="open" href="'+esc(s.browserUrl)+'" target="_blank" rel="noopener">open in browser ↗</a>';
    if (s.joinLink){
      body += '<div class="link"><input readonly value="omp join &quot;'+esc(s.joinLink)+'&quot;">'+
              '<button data-copy="'+esc(s.joinLink)+'">copy</button></div>';
    }
  } else {
    body += '<div class="waiting">waiting for /collab link…</div>';
  }
  body += '<div class="actions"><button class="pane" data-pane="'+esc(s.id)+'">view pane</button></div>';
  el.innerHTML = body;
  el.querySelectorAll('button[data-copy]').forEach(b=>{
    b.onclick = ()=>{ navigator.clipboard.writeText(b.dataset.copy); b.textContent='copied'; setTimeout(()=>b.textContent='copy',1200); };
  });
  el.querySelector('button[data-pane]').onclick = ()=>showPane(s.id, s.name||s.id);
  return el;
}

async function showPane(id, name){
  const modal = document.getElementById('panemodal');
  const pre = document.getElementById('panepre');
  document.getElementById('panetitle').textContent = name;
  pre.textContent = 'loading…';
  modal.hidden = false;
  try {
    const res = await fetch('/api/v1/sessions/'+encodeURIComponent(id)+'/pane?strip=1', {headers:{'Accept':'text/plain'}});
    pre.textContent = res.ok ? (await res.text()) : ('pane unavailable ('+res.status+')');
  } catch(e){ pre.textContent = String(e); }
}

async function refresh(){
  try {
    const res = await fetch('/api/v1/sessions', {headers:{'Accept':'application/json'}});
    if (res.status === 401){ location.href='/auth/login'; return; }
    const sessions = await res.json();
    grid.replaceChildren(...sessions.map(card));
    empty.hidden = sessions.length > 0;
  } catch(e){ /* transient; next tick retries */ }
}

const form = document.getElementById('newform');
if (form){
  form.addEventListener('submit', async (e)=>{
    e.preventDefault();
    const err = document.getElementById('newerr');
    const btn = form.querySelector('button');
    err.textContent = '';
    btn.disabled = true;
    try {
      const res = await fetch('/api/v1/sessions', {
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body: JSON.stringify({
          name: document.getElementById('newname').value,
          dir: document.getElementById('newdir').value,
        }),
      });
      if (!res.ok){ err.textContent = (await res.text()).trim() || ('error '+res.status); return; }
      form.reset();
      setTimeout(refresh, 300);
    } catch(ex){ err.textContent = String(ex); }
    finally { btn.disabled = false; }
  });
}
refresh();
setInterval(refresh, 4000);
</script>
</body>
</html>`))
