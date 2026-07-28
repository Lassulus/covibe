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
  .list { display:flex; flex-direction:column; gap:.35rem; }
  /* The actions track is a fixed width, not auto: an auto track sizes to its
     buttons, which differ per row (QR + open vs. "waiting for host…"), and that
     changes how the fr tracks divide the rest — so nothing would line up. */
  .row { background:var(--card); border:1px solid #222; border-radius:8px; padding:.55rem .75rem;
         display:grid; gap:.5rem 1rem; align-items:center;
         grid-template-columns:minmax(160px,1.4fr) minmax(160px,2fr) minmax(120px,1.2fr) 250px; }
  .row.head { background:transparent; border-color:transparent; padding:.1rem .75rem; color:var(--muted);
              font-size:.72rem; text-transform:uppercase; letter-spacing:.04em; }
  .row .name { font-weight:600; display:flex; align-items:center; gap:.45rem; min-width:0; }
  .row .name span.t { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  @media (max-width:860px) {
    .row { grid-template-columns:1fr auto; }
    .row.head { display:none; }
    .row .dir, .row .who2 { grid-column:1 / -1; }
  }
  .dot { width:.6rem; height:.6rem; border-radius:50%; display:inline-block; }
  .dot.live{background:var(--live)} .dot.starting{background:var(--start)} .dot.ended{background:var(--ended)}
  .meta { color:var(--muted); font-size:.82rem; word-break:break-all; }
  .badge { font-size:.7rem; padding:.1rem .45rem; border-radius:999px; border:1px solid #333; color:var(--muted); }
  .qrpanel { grid-column:1 / -1; display:flex; gap:1rem; align-items:flex-start; flex-wrap:wrap;
             border-top:1px solid #222; margin-top:.35rem; padding-top:.6rem; }
  .qrpanel[hidden] { display:none; }
  .qr { background:#fff; padding:8px; border-radius:8px; }
  .qr img { display:block; width:200px; height:200px; image-rendering:pixelated; }
  .links { flex:1; min-width:240px; display:flex; flex-direction:column; gap:.4rem; }
  .link { display:flex; gap:.4rem; }
  .link input { flex:1; background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); border-radius:6px; padding:.4rem .5rem; font-family:ui-monospace,monospace; font-size:.78rem; }
  button { background:#21262d; color:var(--fg); border:1px solid #30363d; border-radius:6px; padding:.4rem .6rem; cursor:pointer; font-size:.8rem; }
  button:hover { border-color:var(--accent); }
  a.open { color:var(--accent); text-decoration:none; font-size:.85rem; }
  .empty { color:var(--muted); text-align:center; padding:3rem; }
  .waiting { color:var(--start); font-size:.82rem; }
  .newform { display:flex; gap:.5rem; flex-wrap:wrap; align-items:center; margin-bottom:1.25rem; }
  .newform input, .newform select { background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); border-radius:6px; padding:.5rem .6rem; font-size:.85rem; }
  .newform input#newname { min-width:220px; }
  .newform input#newdir { min-width:160px; }
  .newerr { color:#f85149; font-size:.8rem; }
  .actions { display:flex; gap:.35rem; justify-content:flex-end; flex-wrap:wrap; }
  .actions button, .actions a.open { font-size:.78rem; }
  .kill { border-color:#5a2a2a; }
  .modal { position:fixed; inset:0; background:rgba(0,0,0,.6); display:flex; align-items:center; justify-content:center; padding:1.5rem; z-index:10; }
  .modal[hidden] { display:none; }
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
    <select id="newmodel" name="model"><option value="">default model</option></select>
    <select id="newthinking" name="thinking">
      <option value="">thinking: default</option>
      <option value="off">off</option>
      <option value="minimal">minimal</option>
      <option value="low">low</option>
      <option value="medium">medium</option>
      <option value="high">high</option>
      <option value="xhigh">xhigh</option>
      <option value="max">max</option>
    </select>
    <button type="submit">start session</button>
    <span id="newerr" class="newerr"></span>
  </form>
  {{end}}
  <div class="row head"><div>session</div><div>directory</div><div>where · since</div><div class="actions">actions</div></div>
  <div id="list" class="list"></div>
  <div id="empty" class="empty" hidden>No live sessions.{{if not .CanCreate}} Start one with <code>covibe start &lt;name&gt;</code>.{{end}}</div>
</main>
<div id="panemodal" class="modal" hidden>
  <div class="modalbox">
    <div class="modalhead"><strong id="panetitle"></strong><button id="paneclose">close</button></div>
    <pre id="panepre" class="pane-out"></pre>
  </div>
</div>
<script nonce="{{.Nonce}}">
const list = document.getElementById('list');
const empty = document.getElementById('empty');
// Rows are rebuilt wholesale every refresh, so remember which QR panels are
// open — otherwise a QR you opened to scan vanishes on the next tick.
const qrOpen = new Set();
document.getElementById('paneclose').onclick = ()=>{ document.getElementById('panemodal').hidden = true; };

function esc(s){ return String(s??'').replace(/[&<>"']/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }

function row(s){
  const el = document.createElement('div');
  el.className = 'row';
  const started = new Date(s.startedAt).toLocaleTimeString();
  const origin = s.host ? ('@'+esc(s.host)) : (s.mux ? esc(s.mux)+(s.muxSession?' · '+esc(s.muxSession):'') : '');
  const model = s.model ? esc(s.model)+(s.thinking?' · '+esc(s.thinking):'') : (s.thinking?esc(s.thinking):'');
  let body = '';
  body += '<div class="name"><span class="dot '+esc(s.status)+'"></span><span class="t">'+esc(s.name||s.id)+'</span>'+
          (s.viewOnly?' <span class="badge">view-only</span>':'')+'</div>';
  body += '<div class="meta dir">'+esc(s.dir||'')+'</div>';
  body += '<div class="meta who2">'+(origin?origin+' · ':'')+esc(started)+(model?'<br>'+model:'')+'</div>';
  // The QR lives behind a per-row toggle: the overview stays scannable as text.
  body += '<div class="actions">';
  if (s.browserUrl){
    body += '<button data-qr="'+esc(s.id)+'">QR</button>';
    body += '<a class="open" href="'+esc(s.browserUrl)+'" target="_blank" rel="noopener">open ↗</a>';
  } else {
    body += '<span class="waiting">waiting for host…</span>';
  }
  body += '<button class="pane" data-pane="'+esc(s.id)+'">pane</button>'+
          '<button class="kill" data-kill="'+esc(s.id)+'">kill</button></div>';
  if (s.browserUrl){
    body += '<div class="qrpanel" hidden><div class="qr"><img alt="join QR"></div><div class="links">'+
            '<div class="link"><input readonly value="'+esc(s.browserUrl)+'">'+
            '<button data-copy="'+esc(s.browserUrl)+'">copy</button></div>';
    if (s.joinLink){
      body += '<div class="link"><input readonly value="omp join &quot;'+esc(s.joinLink)+'&quot;">'+
              '<button data-copy="omp join &quot;'+esc(s.joinLink)+'&quot;">copy</button></div>';
    }
    body += '</div></div>';
  }
  el.innerHTML = body;
  el.querySelectorAll('button[data-copy]').forEach(b=>{
    b.onclick = ()=>{ navigator.clipboard.writeText(b.dataset.copy); b.textContent='copied'; setTimeout(()=>b.textContent='copy',1200); };
  });
  const qrBtn = el.querySelector('button[data-qr]');
  if (qrBtn){
    const panel = el.querySelector('.qrpanel');
    const img = panel.querySelector('img');
    const show = (on)=>{
      // Rendered on demand from /qr, so the listing carries no QR payload.
      if (on && !img.getAttribute('src')) img.src = '/qr?data='+encodeURIComponent(s.browserUrl)+'&size=240';
      panel.hidden = !on;
      qrBtn.textContent = on ? 'hide QR' : 'QR';
      if (on) qrOpen.add(s.id); else qrOpen.delete(s.id);
    };
    qrBtn.onclick = ()=> show(panel.hidden);
    if (qrOpen.has(s.id)) show(true);
  }
  el.querySelector('button[data-pane]').onclick = ()=>showPane(s.id, s.name||s.id);
  el.querySelector('button[data-kill]').onclick = async (ev)=>{
    if (!confirm('Kill session '+(s.name||s.id)+'?')) return;
    const btn = ev.currentTarget;
    btn.disabled = true;
    try {
      const res = await fetch('/api/v1/sessions/'+encodeURIComponent(s.id), {method:'DELETE'});
      // A stale row can name a session the dashboard no longer has (a remote
      // wrapper re-registers under a new id): say so instead of looking dead.
      if (!res.ok) btn.textContent = res.status===404 ? 'gone — refreshing' : ('kill failed ('+res.status+')');
    } catch(e){ btn.textContent = 'kill failed'; }
    setTimeout(refresh, 300);
  };
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
    list.replaceChildren(...sessions.map(row));
    empty.hidden = sessions.length > 0;
  } catch(e){ /* transient; next tick retries */ }
}

const form = document.getElementById('newform');
if (form){
  const modelSel = document.getElementById('newmodel');
  const thinkSel = document.getElementById('newthinking');
  const THINK_ALL = ['off','minimal','low','medium','high','xhigh','max'];
  const modelThinking = {};
  function fillThinking(levels){
    const cur = thinkSel.value;
    thinkSel.replaceChildren();
    const def = document.createElement('option'); def.value=''; def.textContent='thinking: default';
    thinkSel.appendChild(def);
    (levels && levels.length ? levels : THINK_ALL).forEach(l=>{
      const o=document.createElement('option'); o.value=l; o.textContent=l; thinkSel.appendChild(o);
    });
    thinkSel.value = Array.from(thinkSel.options).some(o=>o.value===cur) ? cur : '';
  }
  modelSel.addEventListener('change', ()=> fillThinking(modelThinking[modelSel.value]));
  fillThinking(THINK_ALL);
  (async ()=>{
    try {
      const res = await fetch('/api/v1/models', {headers:{'Accept':'application/json'}});
      if (!res.ok) return;
      const models = await res.json();
      const groups = {};
      (models||[]).forEach(m=>{
        modelThinking[m.selector] = m.thinking || [];
        (groups[m.provider] = groups[m.provider] || []).push(m);
      });
      Object.keys(groups).sort().forEach(p=>{
        const og = document.createElement('optgroup'); og.label = p;
        groups[p].forEach(m=>{
          const o = document.createElement('option'); o.value = m.selector; o.textContent = m.name || m.selector;
          og.appendChild(o);
        });
        modelSel.appendChild(og);
      });
    } catch(e){ /* leave default-only select */ }
  })();
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
          model: document.getElementById('newmodel').value,
          thinking: document.getElementById('newthinking').value,
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
