package dashboard

import "html/template"

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>covibe — sessions</title>
<style nonce="{{.Nonce}}">
  :root { color-scheme: dark light; --bg:#0e1116; --card:#171b22; --muted:#8b949e; --fg:#e6edf3; --accent:#4ea1ff; --live:#3fb950; --ended:#8b949e; --start:#d29922; --line:#242a33; }
  * { box-sizing: border-box; border-radius:0; }
  body { margin:0; font:15px/1.5 system-ui,sans-serif; background:var(--bg); color:var(--fg); }
  header { display:flex; align-items:center; justify-content:space-between; gap:1rem; padding:1rem 1.5rem; border-bottom:1px solid var(--line); }
  header h1 { font-size:1.1rem; margin:0; letter-spacing:.02em; }
  header .who { color:var(--muted); font-size:.85rem; }
  header a { color:var(--accent); text-decoration:none; }
  main { padding:1.5rem; }
  table.sessions { width:100%; border-collapse:collapse; font-size:.85rem; }
  table.sessions th, table.sessions td { text-align:left; padding:.4rem .6rem; border-bottom:1px solid var(--line); vertical-align:top; }
  table.sessions thead th { color:var(--muted); font-size:.72rem; font-weight:600; text-transform:uppercase; letter-spacing:.04em;
                            white-space:nowrap; user-select:none; border-bottom:1px solid #39404a; }
  table.sessions thead th.sortable { cursor:pointer; }
  table.sessions thead th.sortable:hover { color:var(--fg); }
  table.sessions thead th .arrow { color:var(--accent); }
  table.sessions tbody tr.session:hover { background:var(--card); }
  table.sessions td.name { font-weight:600; white-space:nowrap; }
  table.sessions td.meta { color:var(--muted); word-break:break-all; }
  table.sessions td.num { white-space:nowrap; color:var(--muted); font-variant-numeric:tabular-nums; }
  table.sessions td.actions { text-align:right; white-space:nowrap; }
  .dot { width:.6rem; height:.6rem; border-radius:50%; display:inline-block; margin-right:.35rem; }
  .dot.live{background:var(--live)} .dot.starting{background:var(--start)} .dot.ended{background:var(--ended)}
  .status { white-space:nowrap; color:var(--muted); }
  .badge { font-size:.7rem; padding:0 .3rem; border:1px solid #39404a; color:var(--muted); margin-left:.35rem; }
  .qrpanel td { background:var(--card); }
  .qrbox { display:flex; gap:1rem; align-items:flex-start; flex-wrap:wrap; }
  .qr { background:#fff; padding:8px; }
  .qr img { display:block; width:200px; height:200px; image-rendering:pixelated; }
  .links { flex:1; min-width:240px; display:flex; flex-direction:column; gap:.4rem; }
  .link { display:flex; gap:.4rem; }
  .link input { flex:1; background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); padding:.3rem .45rem; font-family:ui-monospace,monospace; font-size:.78rem; }
  .sharepanel td { background:var(--card); }
  .sharebox { display:flex; flex-direction:column; gap:.4rem; max-width:560px; }
  .sharebox .shead { color:var(--muted); font-size:.78rem; }
  .member { display:flex; align-items:center; gap:.4rem; font-size:.8rem; }
  .member .mkey { color:var(--muted); font-family:ui-monospace,monospace; font-size:.72rem; }
  .member .mdel { border-color:#5a2a2a; padding:0 .4rem; line-height:1.2; }
  .mnone { color:var(--muted); font-size:.78rem; }
  .addrow { display:flex; gap:.4rem; align-items:center; flex-wrap:wrap; }
  .addrow input { background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); padding:.3rem .45rem; font-size:.78rem; min-width:220px; }
  .adderr { color:#f85149; font-size:.78rem; }
  button { background:#21262d; color:var(--fg); border:1px solid #30363d; padding:.25rem .55rem; cursor:pointer; font-size:.78rem; }
  button:hover { border-color:var(--accent); }
  a.open { color:var(--accent); text-decoration:none; font-size:.78rem; margin:0 .35rem; }
  .empty { color:var(--muted); text-align:center; padding:3rem; }
  .waiting { color:var(--start); font-size:.78rem; margin-right:.35rem; }
  .newform { display:flex; gap:.5rem; flex-wrap:wrap; align-items:center; margin-bottom:1.25rem; }
  .newform input, .newform select { background:#0b0e13; border:1px solid #2a2f37; color:var(--fg); padding:.4rem .55rem; font-size:.85rem; }
  .newform input#newname { min-width:220px; }
  .newform input#newdir { min-width:160px; }
  .newerr { color:#f85149; font-size:.8rem; }
  .kill { border-color:#5a2a2a; }
  .modal { position:fixed; inset:0; background:rgba(0,0,0,.6); display:flex; align-items:center; justify-content:center; padding:1.5rem; z-index:10; }
  .modal[hidden] { display:none; }
  .modalbox { background:var(--card); border:1px solid #30363d; width:min(900px,100%); max-height:85vh; display:flex; flex-direction:column; }
  .modalhead { display:flex; justify-content:space-between; align-items:center; padding:.6rem 1rem; border-bottom:1px solid #30363d; }
  .pane-out { margin:0; padding:1rem; overflow:auto; font-family:ui-monospace,monospace; font-size:.78rem; white-space:pre-wrap; word-break:break-word; }
</style>
</head>
<body>
<header>
  <h1>covibe · co-vibing sessions</h1>
  <div>
    {{if .Relay}}<span class="badge">relay {{.Relay}}</span>{{end}}
    <span class="who">{{if .User.Email}}{{.User.Email}}{{else}}{{.User.Sub}}{{end}}</span>
    {{if .IsAdmin}}<span class="badge">admin</span>{{end}}
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
  <table id="sessions" class="sessions" hidden>
    <thead><tr id="head"></tr></thead>
    <tbody id="list"></tbody>
  </table>
  <div id="empty" class="empty" hidden>No live sessions.{{if not .CanCreate}} Start one with <code>covibe start &lt;name&gt;</code>.{{end}}</div>
  {{if .IsAdmin}}<datalist id="userlist"></datalist>{{end}}
</main>
<div id="panemodal" class="modal" hidden>
  <div class="modalbox">
    <div class="modalhead"><strong id="panetitle"></strong><button id="paneclose">close</button></div>
    <pre id="panepre" class="pane-out"></pre>
  </div>
</div>
<script nonce="{{.Nonce}}">
const table = document.getElementById('sessions');
const head = document.getElementById('head');
const list = document.getElementById('list');
const empty = document.getElementById('empty');
// Rows are rebuilt wholesale every refresh, so remember which QR panels are
// open — otherwise a QR you opened to scan vanishes on the next tick.
const qrOpen = new Set();
// Same story for the share panels, plus the half-typed name in their add box.
const shareOpen = new Set();
const shareDraft = new Map();
const IS_ADMIN = {{.IsAdmin}};
document.getElementById('paneclose').onclick = ()=>{ document.getElementById('panemodal').hidden = true; };

function esc(s){ return String(s??'').replace(/[&<>"']/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }

const STATUS_ORDER = {live:0, starting:1, ended:2};
function origin(s){ return s.host ? '@'+s.host : (s.mux ? s.mux+(s.muxSession?' · '+s.muxSession:'') : ''); }
function age(ms){
  const d = Math.max(0, Math.floor((Date.now()-ms)/1000));
  if (d < 60) return d+'s';
  if (d < 3600) return Math.floor(d/60)+'m';
  if (d < 86400) return Math.floor(d/3600)+'h '+Math.floor(d%3600/60)+'m';
  return Math.floor(d/86400)+'d '+Math.floor(d%86400/3600)+'h';
}

// Every column declares how it sorts, so the header is data — adding metadata
// means adding one entry here, not another branch in the comparator.
const COLS = [
  {key:'status',   label:'status',    cls:'status', sort:s=>STATUS_ORDER[s.status] ?? 9,
   cell:s=>'<span class="dot '+esc(s.status)+'"></span>'+esc(s.status)},
  {key:'name',     label:'name',      cls:'name',   sort:s=>(s.name||s.id).toLowerCase(),
   cell:s=>esc(s.name||s.id)+(s.viewOnly?'<span class="badge">view-only</span>':'')},
  {key:'dir',      label:'directory', cls:'meta',   sort:s=>(s.dir||'').toLowerCase(), cell:s=>esc(s.dir||'')},
  {key:'owner',    label:'owner',     cls:'meta',   sort:s=>(s.owner||'').toLowerCase(), cell:s=>esc(s.owner||'')},
  {key:'origin',   label:'where',     cls:'meta',   sort:s=>origin(s).toLowerCase(),   cell:s=>esc(origin(s))},
  {key:'model',    label:'model',     cls:'meta',   sort:s=>(s.model||'').toLowerCase(), cell:s=>esc(s.model||'')},
  {key:'thinking', label:'thinking',  cls:'meta',   sort:s=>(s.thinking||'').toLowerCase(), cell:s=>esc(s.thinking||'')},
  // Quiet by default: an unshared session shows a blank cell, not a zero.
  {key:'members',  label:'shared',    cls:'num',    sort:s=>(s.members||[]).length,
   cell:s=>{ const n = (s.members||[]).length; return n ? esc(n) : ''; }},
  // Age sorts on the raw timestamp: newest first means largest startedAt, so
  // the ascending direction is "youngest" and the label stays honest.
  {key:'age',      label:'age',       cls:'num',    sort:s=>-new Date(s.startedAt).getTime(),
   cell:s=>esc(age(new Date(s.startedAt).getTime()))},
];

let sortKey = localStorage.getItem('sortKey') || 'age';
let sortAsc = localStorage.getItem('sortAsc') !== '0';
if (!COLS.some(c=>c.key===sortKey)) sortKey = 'age';

function renderHead(){
  head.replaceChildren();
  COLS.forEach(c=>{
    const th = document.createElement('th');
    th.className = 'sortable';
    th.textContent = c.label;
    if (c.key === sortKey){
      const a = document.createElement('span');
      a.className = 'arrow';
      a.textContent = sortAsc ? ' ▲' : ' ▼';
      th.appendChild(a);
    }
    th.onclick = ()=>{
      if (sortKey === c.key) sortAsc = !sortAsc; else { sortKey = c.key; sortAsc = true; }
      localStorage.setItem('sortKey', sortKey);
      localStorage.setItem('sortAsc', sortAsc ? '1' : '0');
      renderHead();
      render();
    };
    head.appendChild(th);
  });
  const th = document.createElement('th');
  th.textContent = 'actions';
  th.style.textAlign = 'right';
  head.appendChild(th);
}

function sorted(sessions){
  const col = COLS.find(c=>c.key===sortKey);
  const dir = sortAsc ? 1 : -1;
  return sessions.slice().sort((a,b)=>{
    const x = col.sort(a), y = col.sort(b);
    let d = 0;
    if (typeof x === 'number' && typeof y === 'number') d = x - y;
    else d = String(x).localeCompare(String(y));
    // Name breaks ties so equal keys keep a stable, predictable order.
    return d !== 0 ? d*dir : String(a.name||a.id).localeCompare(String(b.name||b.id));
  });
}

function rowsFor(s){
  const out = [];
  const tr = document.createElement('tr');
  tr.className = 'session';
  let body = '';
  COLS.forEach(c=>{ body += '<td class="'+c.cls+'">'+c.cell(s)+'</td>'; });
  // The QR lives behind a per-row toggle: the overview stays scannable as text.
  body += '<td class="actions">';
  if (s.browserUrl){
    body += '<button data-qr="'+esc(s.id)+'">QR</button>';
    body += '<a class="open" href="'+esc(s.browserUrl)+'" target="_blank" rel="noopener">open ↗</a>';
  } else {
    body += '<span class="waiting">waiting for host…</span>';
  }
  if (s.canManage) body += '<button class="share" data-share="'+esc(s.id)+'">share</button> ';
  body += '<button class="pane" data-pane="'+esc(s.id)+'">pane</button>';
  // Kill answers 403 without manage rights, so don't offer a button that only fails.
  if (s.canManage) body += ' <button class="kill" data-kill="'+esc(s.id)+'">kill</button>';
  body += '</td>';
  tr.innerHTML = body;
  out.push(tr);

  let panel = null;
  if (s.browserUrl){
    panel = document.createElement('tr');
    panel.className = 'qrpanel';
    panel.hidden = true;
    let p = '<td colspan="'+(COLS.length+1)+'"><div class="qrbox"><div class="qr"><img alt="join QR"></div><div class="links">'+
            '<div class="link"><input readonly value="'+esc(s.browserUrl)+'">'+
            '<button data-copy="'+esc(s.browserUrl)+'">copy</button></div>';
    if (s.joinLink){
      p += '<div class="link"><input readonly value="omp join &quot;'+esc(s.joinLink)+'&quot;">'+
           '<button data-copy="omp join &quot;'+esc(s.joinLink)+'&quot;">copy</button></div>';
    }
    panel.innerHTML = p + '</div></div></td>';
    panel.querySelectorAll('button[data-copy]').forEach(b=>{
      b.onclick = ()=>{ navigator.clipboard.writeText(b.dataset.copy); b.textContent='copied'; setTimeout(()=>b.textContent='copy',1200); };
    });
    out.push(panel);
  }

  if (s.canManage){
    const sp = document.createElement('tr');
    sp.className = 'sharepanel';
    sp.hidden = true;
    const members = s.members || [];
    let h = '<td colspan="'+(COLS.length+1)+'"><div class="sharebox">'+
            '<div class="shead">owner: '+esc(s.owner||'—')+' · shared with '+members.length+'</div>';
    if (!members.length) h += '<div class="mnone">not shared with anyone yet</div>';
    members.forEach(m=>{
      h += '<div class="member"><span>'+esc(m.label||m.key)+'</span>';
      if (m.label && m.key && m.label !== m.key) h += '<span class="mkey">'+esc(m.key)+'</span>';
      h += '<button class="mdel" data-del="'+esc(m.key)+'" title="remove">×</button></div>';
    });
    h += '<div class="addrow"><input class="adduser" type="text" placeholder="email or username" autocomplete="off"'+
         (IS_ADMIN ? ' list="userlist"' : '')+'>'+
         '<button class="addbtn">add</button><span class="adderr"></span></div>';
    sp.innerHTML = h + '</div></td>';
    out.push(sp);

    const err = sp.querySelector('.adderr');
    const input = sp.querySelector('input.adduser');
    const addBtn = sp.querySelector('.addbtn');
    input.dataset.sid = s.id;
    input.value = shareDraft.get(s.id) || '';
    input.oninput = ()=> shareDraft.set(s.id, input.value);
    input.onkeydown = (ev)=>{ if (ev.key === 'Enter'){ ev.preventDefault(); addBtn.onclick(); } };
    addBtn.onclick = async ()=>{
      const who = input.value.trim();
      if (!who) return;
      err.textContent = '';
      addBtn.disabled = true;
      try {
        const res = await fetch('/api/v1/sessions/'+encodeURIComponent(s.id)+'/members', {
          method:'POST',
          headers:{'Content-Type':'application/json'},
          body: JSON.stringify({user: who}),
        });
        if (!res.ok){ err.textContent = (await res.text()).trim() || ('error '+res.status); return; }
        input.value = '';
        shareDraft.delete(s.id);
        refresh();
      } catch(ex){ err.textContent = String(ex); }
      finally { addBtn.disabled = false; }
    };
    sp.querySelectorAll('button[data-del]').forEach(b=>{
      b.onclick = async ()=>{
        err.textContent = '';
        b.disabled = true;
        try {
          const res = await fetch('/api/v1/sessions/'+encodeURIComponent(s.id)+'/members/'+encodeURIComponent(b.dataset.del),
                                  {method:'DELETE'});
          if (!res.ok){ err.textContent = (await res.text()).trim() || ('error '+res.status); b.disabled = false; return; }
        } catch(ex){ err.textContent = String(ex); b.disabled = false; return; }
        refresh();
      };
    });

    const shareBtn = tr.querySelector('button[data-share]');
    const showShare = (on)=>{
      sp.hidden = !on;
      shareBtn.textContent = on ? 'hide share' : 'share';
      if (on) shareOpen.add(s.id); else shareOpen.delete(s.id);
    };
    shareBtn.onclick = ()=> showShare(sp.hidden);
    if (shareOpen.has(s.id)) showShare(true);
  }

  const qrBtn = tr.querySelector('button[data-qr]');
  if (qrBtn && panel){
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
  tr.querySelector('button[data-pane]').onclick = ()=>showPane(s.id, s.name||s.id);
  const killBtn = tr.querySelector('button[data-kill]');
  if (killBtn) killBtn.onclick = async (ev)=>{
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
  return out;
}

let current = [];
function render(){
  // A rebuild mid-typing would drop the caret out of a share box, so put it back.
  const act = document.activeElement;
  const focusSid = act && act.classList.contains('adduser') ? act.dataset.sid : null;
  const rows = [];
  sorted(current).forEach(s=>rows.push(...rowsFor(s)));
  list.replaceChildren(...rows);
  if (focusSid){
    list.querySelectorAll('input.adduser').forEach(el=>{
      if (el.dataset.sid !== focusSid) return;
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
    });
  }
  empty.hidden = current.length > 0;
  table.hidden = current.length === 0;
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
    current = await res.json() || [];
    render();
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
// Only admins may list users, so only admins ask — a 403 here would be noise.
if (IS_ADMIN){
  (async ()=>{
    const dl = document.getElementById('userlist');
    if (!dl) return;
    try {
      const res = await fetch('/api/v1/users', {headers:{'Accept':'application/json'}});
      if (!res.ok) return;
      (await res.json() || []).forEach(u=>{
        const o = document.createElement('option');
        o.value = u.key;
        o.textContent = (u.label || u.key) + (u.email && u.email !== u.key ? ' · ' + u.email : '');
        dl.appendChild(o);
      });
    } catch(e){ /* datalist stays empty */ }
  })();
}
renderHead();
refresh();
setInterval(refresh, 4000);
</script>
</body>
</html>`))
