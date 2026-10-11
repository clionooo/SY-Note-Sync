/* ============================================================
   SY Note Sync · 管理后台前端
   纯原生 JS，无框架、无构建，直接由服务端内嵌提供
   ============================================================ */
'use strict';

// ---------------- 全局状态 ----------------
const S = {
  token: localStorage.getItem('sns_token') || '',
  page: (location.hash || '#overview').slice(1),
  data: null,          // /api/admin/data 的返回
  logs: { page: 1, size: 30, total: 0, items: [], device: '', op: '', q: '' },
  notebooks: [],
  currentDoc: null,
  ws: null,
  wsOk: false,
};

// ---------------- 工具 ----------------
const $ = (id) => document.getElementById(id);

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function ts(t) {
  if (!t) return '—';
  const d = new Date(t * 1000);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function ago(t) {
  if (!t) return '从未';
  const s = Math.floor(Date.now() / 1000) - t;
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i === 0 ? 0 : 1) + ' ' + u[i];
}

function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast ' + (kind || '');
  el.innerHTML = esc(msg);
  $('toasts').appendChild(el);
  setTimeout(() => { el.style.opacity = '0'; el.style.transition = 'opacity .3s'; }, 2600);
  setTimeout(() => el.remove(), 3000);
}

// 把块 DOM 里的文本抽出来做摘要显示（后台列表只看内容，不需要渲染 HTML）
function plain(html) {
  if (!html) return '';
  return String(html).replace(/<[^>]*>/g, ' ').replace(/&nbsp;/g, ' ').replace(/\s+/g, ' ').trim();
}

// ---------------- API ----------------
async function api(path, opts) {
  opts = opts || {};
  const headers = Object.assign({ 'X-Admin-Token': S.token }, opts.headers || {});
  if (opts.body) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, {
    method: opts.method || 'GET',
    headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  let data = null;
  try { data = await res.json(); } catch (e) { /* 可能是空响应 */ }
  if (!res.ok) {
    const msg = (data && data.error) || ('HTTP ' + res.status);
    if (res.status === 401) { showGate('管理令牌不正确'); }
    throw new Error(msg);
  }
  return data;
}

// ---------------- 模态框 ----------------
function modal(title, bodyHtml, footHtml, wide) {
  closeModal();
  const root = $('modalRoot');
  root.innerHTML = `
    <div class="modal-mask" id="modalMask">
      <div class="modal${wide ? ' wide' : ''}">
        <div class="modal-head">
          <h3>${esc(title)}</h3><div class="spacer"></div>
          <button class="close-x" onclick="closeModal()">&times;</button>
        </div>
        <div class="modal-body">${bodyHtml}</div>
        ${footHtml ? `<div class="modal-foot">${footHtml}</div>` : ''}
      </div>
    </div>`;
  const mask = $('modalMask');
  mask.addEventListener('click', (e) => { if (e.target === mask) closeModal(); });
}
function closeModal() { $('modalRoot').innerHTML = ''; }
window.closeModal = closeModal;

// ---------------- 登录门 ----------------
function showGate(msg) {
  $('gate').style.display = 'flex';
  if (msg) $('gateErr').textContent = msg;
}
async function tryEnter() {
  const t = $('gateToken').value.trim();
  if (!t) { $('gateErr').textContent = '请输入管理令牌'; return; }
  S.token = t;
  try {
    await api('/api/admin/data');
    localStorage.setItem('sns_token', t);
    $('gate').style.display = 'none';
    $('gateErr').textContent = '';
    boot();
  } catch (e) {
    $('gateErr').textContent = '校验失败：' + e.message;
  }
}

// ---------------- 侧边栏 ----------------
const PAGES = [
  { id: 'overview', name: '概览', ico: '▦', group: '总览' },
  { id: 'devices', name: '设备', ico: '📱', group: '同步' },
  { id: 'logs', name: '变更日志', ico: '≡', group: '同步' },
  { id: 'notebooks', name: '笔记本', ico: '📚', group: '同步' },
  { id: 'assets', name: '附件', ico: '🖼', group: '同步' },
  { id: 'conflicts', name: '冲突', ico: '⚡', group: '数据' },
  { id: 'snapshots', name: '快照', ico: '◈', group: '数据' },
  { id: 'trash', name: '回收站', ico: '🗑', group: '数据' },
  { id: 'shares', name: '分享', ico: '🔗', group: '数据' },
  { id: 'settings', name: '设置', ico: '⚙', group: '系统' },
];

function renderNav() {
  let html = '';
  let lastGroup = '';
  for (const p of PAGES) {
    if (p.group !== lastGroup) { html += `<div class="nav-group">${esc(p.group)}</div>`; lastGroup = p.group; }
    let badge = '';
    if (p.id === 'conflicts') {
      const n = (S.data && S.data.conflicts || []).length;
      if (n) badge = `<span class="nav-badge">${n}</span>`;
    }
    if (p.id === 'devices' && S.data) {
      badge = `<span class="nav-badge gray">${S.data.stats.devices}</span>`;
    }
    html += `<button class="nav-item${S.page === p.id ? ' active' : ''}" data-page="${p.id}">
      <span class="ico">${p.ico}</span><span>${esc(p.name)}</span>${badge}</button>`;
  }
  $('nav').innerHTML = html;
  $('nav').querySelectorAll('[data-page]').forEach(b => {
    b.onclick = () => { location.hash = '#' + b.dataset.page; };
  });
}

// ---------------- 页面路由 ----------------
async function route() {
  S.page = (location.hash || '#overview').slice(1);
  const meta = PAGES.find(p => p.id === S.page) || PAGES[0];
  $('pageTitle').textContent = meta.name;
  renderNav();
  try {
    if (S.page !== 'logs') await refreshData();
    switch (S.page) {
      case 'overview': return renderOverview();
      case 'devices': return renderDevices();
      case 'logs': return renderLogs();
      case 'notebooks': return renderNotebooks();
      case 'assets': return renderAssets();
      case 'conflicts': return renderConflicts();
      case 'snapshots': return renderSnapshots();
      case 'trash': return renderTrash();
      case 'shares': return renderShares();
      case 'settings': return renderSettings();
      default: return renderOverview();
    }
  } catch (e) {
    $('view').innerHTML = `<div class="card"><div class="empty"><span class="big">⚠</span>加载失败：${esc(e.message)}</div></div>`;
  }
}

async function refreshData() {
  S.data = await api('/api/admin/data');
  $('sideFoot').innerHTML = `服务端 v${esc(S.data.version)} · 协议 ${esc(String(S.data.protocol))}<br>令牌 ${esc((S.data.settings && S.data.settings.site_name) || '')}`;
}

// ---------------- 概览 ----------------
function renderOverview() {
  const d = S.data, st = d.stats;
  const devRows = d.devices.map(v => `
    <tr>
      <td>${v.online ? '<span class="tag on">● 在线</span>' : '<span class="tag off">○ 离线</span>'}</td>
      <td><b>${esc(v.name)}</b>${v.note ? `<div class="hint">${esc(v.note)}</div>` : ''}</td>
      <td class="nowrap">${esc(v.platform || '—')} ${v.version ? 'v' + esc(v.version) : ''}</td>
      <td class="nowrap">${ago(v.lastSeen)}</td>
      <td class="mono">${v.changeCount || 0}</td>
    </tr>`).join('') || `<tr><td colspan="5"><div class="empty">还没有设备。在思源插件里填服务地址 + 管理令牌，点「注册新设备」。</div></td></tr>`;

  const logRows = (d.logs || []).slice(0, 12).map(l => `
    <tr>
      <td class="mono">${l.id}</td>
      <td><span class="tag ${esc(l.op)}">${esc(l.op)}</span></td>
      <td class="mono" title="${esc(l.blockId)}">${esc(l.blockId.slice(0, 12))}…</td>
      <td>${esc(l.deviceName || ('#' + l.deviceId))}</td>
      <td class="nowrap">${ts(l.ts)}</td>
      <td class="mono">${esc(plain(l.content).slice(0, 40))}</td>
    </tr>`).join('') || `<tr><td colspan="6"><div class="empty">暂无变更记录</div></td></tr>`;

  $('view').innerHTML = `
    <div class="grid stats" style="margin-bottom:18px">
      <div class="stat accent"><div class="k">📱 设备</div><div class="v">${st.devices}</div><div class="s">${st.online} 台在线</div></div>
      <div class="stat"><div class="k">🧩 同步块</div><div class="v">${st.blocks}</div><div class="s">已删除 ${st.deleted}</div></div>
      <div class="stat"><div class="k">≡ 累计变更</div><div class="v">${st.changes}</div><div class="s">最近 ${ago(st.lastChange)}</div></div>
      <div class="stat"><div class="k">⚡ 待处理冲突</div><div class="v" style="color:${st.conflicts ? 'var(--danger)' : 'inherit'}">${st.conflicts}</div><div class="s">需要人工裁决</div></div>
      <div class="stat"><div class="k">📚 笔记本 / 文档</div><div class="v">${st.notebooks}<span style="font-size:16px;color:var(--text-3)"> / ${st.docs}</span></div><div class="s">由插件上报</div></div>
      <div class="stat"><div class="k">◈ 快照</div><div class="v">${st.snapshots}</div><div class="s">可整库回滚</div></div>
      <div class="stat"><div class="k">🔗 分享</div><div class="v">${st.shares}</div><div class="s">公开只读链接</div></div>
      <div class="stat"><div class="k">🖼 附件</div><div class="v">${st.assets || 0}</div><div class="s">合计 ${bytes(st.assetBytes || 0)}</div></div>
      <div class="stat"><div class="k">💾 数据库</div><div class="v" style="font-size:19px">${bytes(st.dbBytes)}</div><div class="s">SQLite</div></div>
    </div>

    <div class="card">
      <div class="card-head"><h2>设备状态</h2><div class="spacer"></div>
        <button class="btn sm" onclick="location.hash='#devices'">管理设备</button></div>
      <div class="card-body tight"><table><thead><tr>
        <th>状态</th><th>名称</th><th>平台</th><th>最近活跃</th><th>变更数</th>
      </tr></thead><tbody>${devRows}</tbody></table></div>
    </div>

    <div class="card">
      <div class="card-head"><h2>最近变更</h2><div class="spacer"></div>
        <button class="btn sm" onclick="location.hash='#logs'">全部日志</button></div>
      <div class="card-body tight"><table><thead><tr>
        <th>#</th><th>操作</th><th>块 ID</th><th>设备</th><th>时间</th><th>内容</th>
      </tr></thead><tbody>${logRows}</tbody></table></div>
    </div>`;
}

// ---------------- 设备 ----------------
function renderDevices() {
  const rows = S.data.devices.map(v => `
    <tr>
      <td>${v.online ? '<span class="tag on">● 在线</span>' : '<span class="tag off">○ 离线</span>'}</td>
      <td><b>${esc(v.name)}</b>${v.note ? `<div class="hint">${esc(v.note)}</div>` : ''}</td>
      <td class="nowrap">${esc(v.platform || '—')}</td>
      <td class="nowrap">${v.version ? 'v' + esc(v.version) : '—'}</td>
      <td class="nowrap">${ts(v.created)}</td>
      <td class="nowrap">${ago(v.lastSeen)}</td>
      <td class="mono">${v.changeCount || 0}</td>
      <td class="mono">${v.blockCount || 0}</td>
      <td class="nowrap">
        <button class="btn sm" data-act="key" data-id="${v.id}">密钥</button>
        <button class="btn sm" data-act="edit" data-id="${v.id}">改名</button>
        <button class="btn sm" data-act="rotate" data-id="${v.id}">换密钥</button>
        <button class="btn sm danger" data-act="del" data-id="${v.id}">删除</button>
      </td>
    </tr>`).join('') || `<tr><td colspan="9"><div class="empty"><span class="big">📱</span>还没有设备</div></td></tr>`;

  $('view').innerHTML = `
    <div class="card">
      <div class="card-head">
        <h2>设备列表</h2><div class="spacer"></div>
        <span class="hint" style="margin:0">设备密钥是每台设备的通行证；手机端注册失败时，可以在这里「密钥」里查看后手填。</span>
      </div>
      <div class="card-body tight"><div class="tbl-wrap"><table><thead><tr>
        <th>状态</th><th>名称</th><th>平台</th><th>版本</th><th>注册时间</th><th>最近活跃</th><th>变更</th><th>块</th><th>操作</th>
      </tr></thead><tbody>${rows}</tbody></table></div></div>
    </div>`;

  $('view').querySelectorAll('[data-act]').forEach(b => {
    b.onclick = () => deviceAction(b.dataset.act, Number(b.dataset.id));
  });
}

async function deviceAction(act, id) {
  const dev = S.data.devices.find(v => v.id === id);
  if (!dev) return;
  if (act === 'key') {
    const r = await api('/api/admin/device/key?id=' + id);
    modal('设备密钥 · ' + dev.name, `
      <div class="field">
        <label>设备密钥（填进思源插件的「设备密钥」输入框）</label>
        <input class="input mono" value="${esc(r.key)}" readonly onclick="this.select()">
        <div class="hint">点一下输入框会自动全选，方便复制。密钥等同密码，不要外发。</div>
      </div>`, `<button class="btn" onclick="closeModal()">关闭</button>`);
  } else if (act === 'edit') {
    modal('重命名设备', `
      <div class="field"><label>设备名称</label><input id="dvName" class="input" value="${esc(dev.name)}"></div>
      <div class="field"><label>备注</label><input id="dvNote" class="input" value="${esc(dev.note || '')}" placeholder="如：我的 iPhone"></div>`,
      `<button class="btn" onclick="closeModal()">取消</button>
       <button class="btn pri" id="dvSave">保存</button>`);
    $('dvSave').onclick = async () => {
      await api('/api/admin/device/update', { method: 'POST', body: { id, name: $('dvName').value.trim(), note: $('dvNote').value.trim() } });
      closeModal(); toast('已保存', 'ok'); route();
    };
  } else if (act === 'rotate') {
    modal('重新生成设备密钥', `
      <p>设备 <b>${esc(dev.name)}</b> 的旧密钥会<b>立即失效</b>，那台设备需要重新填新密钥才能同步。</p>
      <div class="hint">适合「怀疑密钥泄露」或「手机端密钥丢了想换一把」。</div>`,
      `<button class="btn" onclick="closeModal()">取消</button>
       <button class="btn pri" id="dvRot">确认重置</button>`);
    $('dvRot').onclick = async () => {
      const r = await api('/api/admin/device/rotate', { method: 'POST', body: { id } });
      modal('新密钥 · ' + dev.name, `
        <div class="field"><label>新设备密钥</label>
          <input class="input mono" value="${esc(r.key)}" readonly onclick="this.select()"></div>
        <div class="hint">请立刻复制并填到那台设备的插件设置里。</div>`,
        `<button class="btn pri" onclick="closeModal();route()">我已复制</button>`);
    };
  } else if (act === 'del') {
    modal('删除设备', `<p>确定删除设备 <b>${esc(dev.name)}</b> 吗？</p>
      <div class="hint">只删除设备记录，该设备已经同步上来的笔记内容会保留。</div>`,
      `<button class="btn" onclick="closeModal()">取消</button>
       <button class="btn danger" id="dvDel">删除</button>`);
    $('dvDel').onclick = async () => {
      await api('/api/admin/device/delete', { method: 'POST', body: { id } });
      closeModal(); toast('已删除', 'ok'); route();
    };
  }
}

// ---------------- 变更日志 ----------------
async function renderLogs() {
  const q = new URLSearchParams({
    page: S.logs.page, size: S.logs.size,
    device: S.logs.device, op: S.logs.op, q: S.logs.q,
  });
  const r = await api('/api/admin/logs?' + q.toString());
  S.logs.items = r.logs || [];
  S.logs.total = r.total || 0;

  const opts = ['', 'insert', 'update', 'delete'].map(o =>
    `<option value="${o}"${S.logs.op === o ? ' selected' : ''}>${o || '全部操作'}</option>`).join('');
  const devOpts = ['<option value="">全部设备</option>'].concat(
    (S.data.devices || []).map(v => `<option value="${v.id}"${String(S.logs.device) === String(v.id) ? ' selected' : ''}>${esc(v.name)}</option>`)
  ).join('');

  const rows = S.logs.items.map(l => `
    <tr>
      <td class="mono">${l.id}</td>
      <td><span class="tag ${esc(l.op)}">${esc(l.op)}</span></td>
      <td class="mono"><a href="javascript:void(0)" data-block="${esc(l.blockId)}" title="${esc(l.blockId)}">${esc(l.blockId.slice(0, 14))}…</a></td>
      <td class="mono">${esc((l.docId || '').slice(0, 12))}</td>
      <td>${esc(l.deviceName || ('#' + l.deviceId))}</td>
      <td class="nowrap">${ts(l.ts)}</td>
      <td class="mono">${esc(plain(l.content).slice(0, 60))}</td>
    </tr>`).join('') || `<tr><td colspan="7"><div class="empty">没有匹配的记录</div></td></tr>`;

  const pages = Math.max(1, Math.ceil(S.logs.total / S.logs.size));
  $('view').innerHTML = `
    <div class="toolbar">
      <input id="lgQ" class="input" style="max-width:220px" placeholder="搜块 ID / 文档 / 内容" value="${esc(S.logs.q)}">
      <select id="lgDev" class="input" style="max-width:160px">${devOpts}</select>
      <select id="lgOp" class="input" style="max-width:130px">${opts}</select>
      <button class="btn pri" id="lgGo">筛选</button>
      <div class="spacer"></div>
      <span class="hint" style="margin:0">共 ${S.logs.total} 条</span>
    </div>
    <div class="card"><div class="card-body tight"><div class="tbl-wrap"><table><thead><tr>
      <th>#</th><th>操作</th><th>块 ID</th><th>文档</th><th>设备</th><th>时间</th><th>内容</th>
    </tr></thead><tbody>${rows}</tbody></table></div></div>
      <div class="card-head" style="border-top:1px solid var(--border);border-bottom:none">
        <span class="hint" style="margin:0">第 ${S.logs.page} / ${pages} 页</span><div class="spacer"></div>
        <button class="btn sm" id="lgPrev" ${S.logs.page <= 1 ? 'disabled' : ''}>上一页</button>
        <button class="btn sm" id="lgNext" ${S.logs.page >= pages ? 'disabled' : ''}>下一页</button>
      </div>
    </div>`;

  $('lgGo').onclick = () => {
    S.logs.q = $('lgQ').value.trim();
    S.logs.device = $('lgDev').value;
    S.logs.op = $('lgOp').value;
    S.logs.page = 1;
    renderLogs();
  };
  $('lgQ').onkeydown = (e) => { if (e.key === 'Enter') $('lgGo').click(); };
  if ($('lgPrev')) $('lgPrev').onclick = () => { S.logs.page--; renderLogs(); };
  if ($('lgNext')) $('lgNext').onclick = () => { S.logs.page++; renderLogs(); };
  $('view').querySelectorAll('[data-block]').forEach(a => {
    a.onclick = () => showBlockHistory(a.dataset.block);
  });
}

async function showBlockHistory(blockId) {
  const r = await api('/api/admin/block/history?blockId=' + encodeURIComponent(blockId));
  const items = (r.history || []).map(h => `
    <div class="ver" style="margin-bottom:10px">
      <h4>#${h.id} · ${esc(h.op)} · ${esc(h.deviceName || ('设备' + h.deviceId))} · ${ts(h.ts)}</h4>
      <pre>${esc(h.content || '(无内容)')}</pre>
      <div style="margin-top:8px"><button class="btn sm" data-restore="${h.id}">恢复这一版</button></div>
    </div>`).join('') || '<div class="empty">该块没有历史记录</div>';
  modal('块历史 · ' + blockId, `
    <div class="hint" style="margin-bottom:10px">当前状态：${r.exists ? (r.block.deleted ? '已删除' : '存在') + ' · 版本 ' + r.block.rev : '服务端没有这个块'}</div>
    ${items}`, `<button class="btn" onclick="closeModal()">关闭</button>`, true);
  $('modalRoot').querySelectorAll('[data-restore]').forEach(b => {
    b.onclick = async () => {
      await api('/api/admin/block/restore', { method: 'POST', body: { changeId: Number(b.dataset.restore) } });
      toast('已恢复，并推送给在线设备', 'ok');
      closeModal(); route();
    };
  });
}

// ---------------- 笔记本 ----------------
async function renderNotebooks() {
  const r = await api('/api/admin/notebooks');
  S.notebooks = r.notebooks || [];
  const cards = S.notebooks.map(n => `
    <div class="card" style="margin-bottom:0">
      <div class="card-head"><h2>${esc(n.icon || '📘')} ${esc(n.name || n.id)}</h2><div class="spacer"></div>
        <span class="tag">${n.docs} 文档</span><span class="tag">${n.blocks} 块</span></div>
      <div class="card-body tight"><div id="nb-${esc(n.id)}"><div class="empty">展开查看文档…</div></div></div>
    </div>`).join('');

  $('view').innerHTML = `
    <div class="toolbar"><span class="hint" style="margin:0">笔记本结构由思源插件上报（插件会定时上报当前工作空间的笔记本与文档列表）。</span></div>
    ${S.notebooks.length ? `<div class="grid two">${cards}</div>` :
      `<div class="card"><div class="empty"><span class="big">📚</span>还没有笔记本数据。插件升级到 v2 并连上服务后会自动上报。</div></div>`}`;

  for (const n of S.notebooks) {
    const holder = $('nb-' + n.id);
    if (!holder) continue;
    try {
      const dr = await api('/api/admin/docs?notebook=' + encodeURIComponent(n.id));
      const docs = dr.docs || [];
      holder.innerHTML = docs.length ? `<table><thead><tr><th>文档</th><th>块</th><th>更新时间</th><th></th></tr></thead><tbody>` +
        docs.map(d => `<tr>
          <td>${esc(d.title || d.id)}</td>
          <td class="mono">${d.blocks}</td>
          <td class="nowrap">${ts(d.updated)}</td>
          <td><button class="btn sm" data-doc="${esc(d.id)}">看内容</button>
              <button class="btn sm" data-share="${esc(d.id)}">分享</button></td>
        </tr>`).join('') + `</tbody></table>` : `<div class="empty">这个笔记本下没有文档</div>`;
    } catch (e) {
      holder.innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`;
    }
  }
  $('view').querySelectorAll('[data-doc]').forEach(b => b.onclick = () => showDoc(b.dataset.doc));
  $('view').querySelectorAll('[data-share]').forEach(b => b.onclick = () => shareDialog('', b.dataset.share));
}

async function showDoc(docId) {
  const r = await api('/api/admin/doc/blocks?doc=' + encodeURIComponent(docId));
  const blocks = (r.blocks || []).filter(b => !b.deleted);
  const html = blocks.map(b => `<pre class="block">${esc(plain(b.content) || '(空块)')}</pre>`).join('')
    || '<div class="empty">该文档在服务端没有块</div>';
  modal('文档内容 · ' + docId, `
    <div class="hint" style="margin-bottom:10px">共 ${blocks.length} 个块（显示的是块内纯文本）</div>${html}`,
    `<button class="btn" onclick="closeModal()">关闭</button>
     <button class="btn pri" onclick="shareDialog('', '${esc(docId)}')">生成分享链接</button>`, true);
}

// ---------------- 附件 ----------------
function isImage(p) { return /\.(png|jpe?g|gif|webp|svg|bmp|avif)$/i.test(p); }

function renderAssets() {
  $('view').innerHTML = `
    <div class="toolbar">
      <input id="asQ" class="input" style="max-width:260px" placeholder="按路径筛选">
      <button class="btn" id="asReload">刷新</button>
      <div class="spacer"></div>
      <span class="hint" style="margin:0" id="asSummary">加载中…</span>
    </div>
    <div class="card"><div class="card-body tight"><div class="tbl-wrap"><table><thead><tr>
      <th style="width:64px">预览</th><th>路径</th><th>大小</th><th>类型</th><th>更新时间</th><th>操作</th>
    </tr></thead><tbody id="asBody"><tr><td colspan="6"><div class="empty">加载中…</div></td></tr></tbody></table></div></div></div>`;

  const load = async () => {
    const r = await api('/api/admin/assets?limit=2000');
    const kw = ($('asQ').value || '').trim().toLowerCase();
    const list = (r.assets || []).filter(a => !kw || a.path.toLowerCase().includes(kw));
    $('asSummary').textContent = `共 ${r.count} 个附件 · 合计 ${bytes(r.bytes)}${kw ? ` · 筛选出 ${list.length} 个` : ''}`;
    $('asBody').innerHTML = list.map(a => `
      <tr>
        <td>${isImage(a.path)
          ? `<img src="/api/admin/asset/download?path=${encodeURIComponent(a.path)}&token=${encodeURIComponent(S.token)}" style="width:48px;height:48px;object-fit:cover;border-radius:6px;border:1px solid var(--border)" loading="lazy">`
          : '<span class="tag">文件</span>'}</td>
        <td class="mono" style="word-break:break-all">${esc(a.path)}</td>
        <td class="nowrap">${bytes(a.size)}</td>
        <td class="nowrap">${esc((a.mime || '').split('/')[1] || '—')}</td>
        <td class="nowrap">${ts(a.updated)}</td>
        <td class="nowrap">
          <a class="btn sm" href="/api/admin/asset/download?path=${encodeURIComponent(a.path)}&token=${encodeURIComponent(S.token)}" target="_blank">打开</a>
          <button class="btn sm danger" data-adel="${esc(a.path)}">删除</button>
        </td>
      </tr>`).join('') || `<tr><td colspan="6"><div class="empty"><span class="big">🖼</span>还没有附件。<br>在思源里插入图片后，插件会自动把图片传到服务端。</div></td></tr>`;
    $('asBody').querySelectorAll('[data-adel]').forEach(b => {
      b.onclick = async () => {
        if (!confirm('删除服务端附件 ' + b.dataset.adel + ' ？\n（只从服务端移除，各设备本地已下载的文件会保留）')) return;
        await api('/api/admin/asset/delete', { method: 'POST', body: { paths: [b.dataset.adel] } });
        toast('已删除', 'ok'); load();
      };
    });
  };
  $('asQ').oninput = load;
  $('asReload').onclick = load;
  load();
}

// ---------------- 冲突 ----------------
function renderConflicts() {
  const list = S.data.conflicts || [];
  const items = list.map(c => `
    <div class="card">
      <div class="card-head">
        <h2>块 <code>${esc(c.blockId)}</code></h2><div class="spacer"></div>
        <span class="tag off">${ts(c.created)}</span>
      </div>
      <div class="card-body">
        <div class="versions">
          <div class="ver">
            <h4>版本 A · 服务端当前（rev ${c.versionA.rev}）</h4>
            <pre>${esc(c.versionA.content || '(空)')}</pre>
            <div style="margin-top:9px"><button class="btn sm pri" data-cf="${c.id}" data-ch="0">保留 A</button></div>
          </div>
          <div class="ver">
            <h4>版本 B · 后推送的设备（rev ${c.versionB.rev}）</h4>
            <pre>${esc(c.versionB.content || '(空)')}</pre>
            <div style="margin-top:9px"><button class="btn sm pri" data-cf="${c.id}" data-ch="1">保留 B</button></div>
          </div>
        </div>
      </div>
    </div>`).join('');

  $('view').innerHTML = list.length ? items :
    `<div class="card"><div class="empty"><span class="big">✅</span>没有待处理的冲突</div></div>`;

  $('view').querySelectorAll('[data-cf]').forEach(b => {
    b.onclick = async () => {
      await api('/api/admin/conflict/resolve', {
        method: 'POST',
        body: { conflictId: Number(b.dataset.cf), chosenChangeId: Number(b.dataset.ch) },
      });
      toast('冲突已解决，结果已推送给在线设备', 'ok');
      route();
    };
  });
}

// ---------------- 快照 ----------------
function renderSnapshots() {
  const list = S.data.snapshots || [];
  const rows = list.map(s => `
    <tr>
      <td class="mono">${s.id}</td>
      <td>${esc(s.name)} ${s.auto ? '<span class="tag">自动</span>' : ''}</td>
      <td class="mono">${s.blocks}</td>
      <td class="nowrap">${ts(s.created)}</td>
      <td>${esc(s.note || '—')}</td>
      <td class="nowrap">
        <button class="btn sm" data-rb="${s.id}">回滚到此</button>
        <button class="btn sm danger" data-ds="${s.id}">删除</button>
      </td>
    </tr>`).join('') || `<tr><td colspan="6"><div class="empty">还没有快照</div></td></tr>`;

  $('view').innerHTML = `
    <div class="toolbar">
      <input id="snName" class="input" style="max-width:240px" placeholder="快照名称（如：整理前）">
      <button class="btn pri" id="snNew">创建当前快照</button>
      <div class="spacer"></div>
      <span class="hint" style="margin:0">回滚会把服务端的块状态整体还原，并通知所有在线设备重新全量同步。</span>
    </div>
    <div class="card"><div class="card-body tight"><table><thead><tr>
      <th>#</th><th>名称</th><th>块数</th><th>创建时间</th><th>备注</th><th>操作</th>
    </tr></thead><tbody>${rows}</tbody></table></div></div>`;

  $('snNew').onclick = async () => {
    const name = $('snName').value.trim() || '手动快照';
    await api('/api/admin/snapshot/create', { method: 'POST', body: { name } });
    toast('快照已创建', 'ok'); route();
  };
  $('view').querySelectorAll('[data-ds]').forEach(b => {
    b.onclick = async () => {
      await api('/api/admin/snapshot/delete', { method: 'POST', body: { id: Number(b.dataset.ds) } });
      toast('已删除', 'ok'); route();
    };
  });
  $('view').querySelectorAll('[data-rb]').forEach(b => {
    b.onclick = () => {
      modal('回滚到快照', `<p>确定回滚到快照 <b>#${b.dataset.rb}</b> 吗？</p>
        <div class="hint">服务端的块状态会整体还原到那一刻，所有在线设备会收到通知并重新全量同步。当前较新的修改不会自动备份——建议先创建一个新快照。</div>`,
        `<button class="btn" onclick="closeModal()">取消</button>
         <button class="btn danger" id="rbGo">确认回滚</button>`);
      $('rbGo').onclick = async () => {
        await api('/api/admin/snapshot/rollback', { method: 'POST', body: { id: Number(b.dataset.rb) } });
        closeModal(); toast('已回滚并通知设备', 'ok'); route();
      };
    };
  });
}

// ---------------- 回收站 ----------------
function renderTrash() {
  $('view').innerHTML = `
    <div class="toolbar"><span class="hint" style="margin:0">被删除的块会永久留痕在这里，随时可以恢复；恢复后会广播给所有在线设备。</span></div>
    <div class="card"><div class="card-body tight"><div class="tbl-wrap"><table><thead><tr>
      <th>块 ID</th><th>文档</th><th>删除设备</th><th>删除时间</th><th>内容</th><th>操作</th>
    </tr></thead><tbody id="trashBody"><tr><td colspan="6"><div class="empty">加载中…</div></td></tr></tbody></table></div></div></div>`;
  api('/api/admin/trash').then(r => {
    const list = r.trash || [];
    $('trashBody').innerHTML = list.map(t => `
      <tr>
        <td class="mono">${esc(t.blockId)}</td>
        <td class="mono">${esc((t.docId || '').slice(0, 12))}</td>
        <td>${esc(t.deviceName || ('#' + t.deviceId))}</td>
        <td class="nowrap">${ts(t.ts)}</td>
        <td class="mono">${esc(plain(t.content).slice(0, 60))}</td>
        <td><button class="btn sm" data-rs="${t.id}">恢复</button></td>
      </tr>`).join('') || `<tr><td colspan="6"><div class="empty">回收站是空的</div></td></tr>`;
    $('trashBody').querySelectorAll('[data-rs]').forEach(b => {
      b.onclick = async () => {
        await api('/api/admin/block/restore', { method: 'POST', body: { changeId: Number(b.dataset.rs) } });
        toast('已恢复', 'ok'); renderTrash();
      };
    });
  });
}

// ---------------- 分享 ----------------
function renderShares() {
  const list = S.data.shares || [];
  const base = location.origin;
  const rows = list.map(s => `
    <tr>
      <td>${esc(s.title || '未命名')}</td>
      <td class="mono">${esc(s.blockId || s.docId || '')}</td>
      <td>${s.password ? '<span class="tag">有密码</span>' : '<span class="tag off">公开</span>'}</td>
      <td class="mono">${s.views}</td>
      <td class="nowrap">${s.expires ? ts(s.expires) : '永久'}</td>
      <td class="nowrap">
        <button class="btn sm" data-copy="${base}/share/${esc(s.token)}">复制链接</button>
        <a class="btn sm" href="/share/${esc(s.token)}" target="_blank">打开</a>
        <button class="btn sm danger" data-del="${s.id}">删除</button>
      </td>
    </tr>`).join('') || `<tr><td colspan="6"><div class="empty">还没有分享链接</div></td></tr>`;

  $('view').innerHTML = `
    <div class="toolbar">
      <button class="btn pri" id="shNew">新建分享</button>
      <div class="spacer"></div>
      <span class="hint" style="margin:0">分享是公开只读链接，可设密码与有效期；访问次数会统计。</span>
    </div>
    <div class="card"><div class="card-body tight"><table><thead><tr>
      <th>标题</th><th>对象</th><th>访问</th><th>访问数</th><th>过期</th><th>操作</th>
    </tr></thead><tbody>${rows}</tbody></table></div></div>`;

  $('shNew').onclick = () => shareDialog();
  $('view').querySelectorAll('[data-copy]').forEach(b => {
    b.onclick = () => {
      navigator.clipboard.writeText(b.dataset.copy).then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动复制', 'err'));
    };
  });
  $('view').querySelectorAll('[data-del]').forEach(b => {
    b.onclick = async () => {
      await api('/api/admin/share/delete', { method: 'POST', body: { id: Number(b.dataset.del) } });
      toast('已删除', 'ok'); route();
    };
  });
}

function shareDialog(blockId, docId) {
  modal('新建分享链接', `
    <div class="field"><label>标题</label><input id="shTitle" class="input" placeholder="显示在分享页上的标题"></div>
    <div class="field"><label>分享对象</label>
      <input id="shBlock" class="input mono" placeholder="块 ID（可选）" value="${esc(blockId || '')}">
      <div style="height:8px"></div>
      <input id="shDoc" class="input mono" placeholder="文档 ID（可选，分享整篇文档）" value="${esc(docId || '')}">
      <div class="hint">两个至少填一个。填文档 ID 会分享该文档下所有块。</div>
    </div>
    <div class="field"><label>访问密码（可选）</label><input id="shPwd" class="input" placeholder="留空表示公开"></div>
    <div class="field"><label>有效期（天，0 = 永久）</label><input id="shExp" class="input" type="number" value="0"></div>`,
    `<button class="btn" onclick="closeModal()">取消</button>
     <button class="btn pri" id="shGo">创建</button>`);
  $('shGo').onclick = async () => {
    const body = {
      title: $('shTitle').value.trim(),
      blockId: $('shBlock').value.trim(),
      docId: $('shDoc').value.trim(),
      password: $('shPwd').value.trim(),
      expiresDays: Number($('shExp').value) || 0,
    };
    if (!body.blockId && !body.docId) { toast('至少填一个块或文档 ID', 'err'); return; }
    const r = await api('/api/admin/share/create', { method: 'POST', body });
    const url = location.origin + '/share/' + r.share.token;
    modal('分享已创建', `
      <div class="field"><label>分享链接</label>
        <input class="input mono" value="${esc(url)}" readonly onclick="this.select()"></div>
      <div class="hint">${r.share.password ? '访问需要密码。' : '公开访问，任何人拿到链接都能看。'}</div>`,
      `<button class="btn pri" onclick="closeModal();location.hash='#shares'">好</button>`);
  };
}

// ---------------- 设置 ----------------
function renderSettings() {
  const st = S.data.settings || {};
  const on = (v, def) => (v === undefined || v === '' ? def : v);
  $('view').innerHTML = `
    <div class="card">
      <div class="card-head"><h2>服务端设置</h2></div>
      <div class="card-body">
        <div class="field"><label>站点名称</label>
          <input id="stName" class="input" value="${esc(on(st.site_name, 'SY Note Sync'))}">
          <div class="hint">显示在分享页与后台标题。</div></div>
        <div class="field"><label>允许新设备注册</label>
          <select id="stReg" class="input">
            <option value="1"${on(st.allow_register, '1') === '1' ? ' selected' : ''}>允许</option>
            <option value="0"${on(st.allow_register, '1') === '0' ? ' selected' : ''}>禁止（已有设备不受影响）</option>
          </select></div>
        <div class="field"><label>允许创建分享链接</label>
          <select id="stShare" class="input">
            <option value="1"${on(st.share_enabled, '1') === '1' ? ' selected' : ''}>允许</option>
            <option value="0"${on(st.share_enabled, '1') === '0' ? ' selected' : ''}>禁止</option>
          </select></div>
        <div class="field"><label>自动快照保留数量</label>
          <input id="stSnap" class="input" type="number" value="${esc(on(st.snapshot_keep, '10'))}">
          <div class="hint">只清理「自动」快照，手动创建的快照永远保留。</div></div>
        <div class="field"><label>管理令牌</label>
          <input class="input mono" value="${esc(S.data.adminTokenMasked || '')}" readonly>
          <div class="hint">令牌来自 <code>docker-compose.yml</code> 的 <code>SYNCSRV_ADMIN_TOKEN</code>；
            没配的话服务端首次启动会自动生成一个随机令牌并打印在容器日志里
            （<code>docker compose logs sy-note-sync | grep 管理令牌</code>），重启不会变。
            改令牌要改 compose 后重建容器。</div></div>
        <button class="btn pri" id="stSave">保存设置</button>
      </div>
    </div>

    <div class="card">
      <div class="card-head"><h2>数据与运维</h2></div>
      <div class="card-body">
        <div class="toolbar" style="margin:0">
          <a class="btn" href="/api/admin/export?token=${encodeURIComponent(S.token)}">导出 JSON 备份</a>
          <button class="btn" id="stReload">重新加载后台数据</button>
        </div>
        <div class="hint" style="margin-top:10px">
          数据库文件在容器的 <code>/app/data/sync.db</code>，对应宿主机挂载目录里的 <code>data/</code>。
          备份那个目录 = 备份全部同步数据（拷贝前建议先停容器）。
        </div>
      </div>
    </div>

    <div class="card">
      <div class="card-head"><h2>关于</h2></div>
      <div class="card-body">
        <table>
          <tr><th style="width:160px">服务端版本</th><td class="mono">v${esc(S.data.version)}</td></tr>
          <tr><th>同步协议版本</th><td class="mono">${esc(String(S.data.protocol))}</td></tr>
          <tr><th>最低插件版本</th><td class="mono">v${esc('2.0.0')}</td></tr>
          <tr><th>项目</th><td><a href="https://github.com/clionooo/SY-Note-Sync" target="_blank">github.com/clionooo/SY-Note-Sync</a></td></tr>
        </table>
      </div>
    </div>`;

  $('stSave').onclick = async () => {
    await api('/api/admin/settings', {
      method: 'POST',
      body: {
        site_name: $('stName').value.trim(),
        allow_register: $('stReg').value,
        share_enabled: $('stShare').value,
        snapshot_keep: $('stSnap').value,
      },
    });
    toast('设置已保存', 'ok'); route();
  };
  $('stReload').onclick = () => route();
}

// ---------------- 实时连接 ----------------
function connectWS() {
  if (S.ws) { try { S.ws.close(); } catch (e) {} }
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${proto}//${location.host}/api/admin/ws?token=${encodeURIComponent(S.token)}`);
  S.ws = ws;
  ws.onopen = () => { S.wsOk = true; paintWs(); };
  ws.onclose = () => { S.wsOk = false; paintWs(); setTimeout(connectWS, 5000); };
  ws.onerror = () => { S.wsOk = false; paintWs(); };
  ws.onmessage = (ev) => {
    let m = null;
    try { m = JSON.parse(ev.data); } catch (e) { return; }
    if (!m || m.type === 'hello') return;
    // 设备上下线 / 新变更 / 新冲突 → 刷新当前页
    const relevant = ['device_online', 'device_added', 'changes', 'conflict'];
    if (relevant.includes(m.type)) {
      if (S.page === 'logs') {
        api('/api/admin/logs?' + new URLSearchParams({ page: S.logs.page, size: S.logs.size, device: S.logs.device, op: S.logs.op, q: S.logs.q }))
          .then(r => { S.logs.items = r.logs || []; S.logs.total = r.total || 0; renderLogs(); }).catch(() => {});
        return;
      }
      refreshData().then(route).catch(() => {});
    }
  };
}
function paintWs() {
  const d = $('wsDot'), t = $('wsText');
  if (S.wsOk) { d.className = 'dot on'; t.textContent = '实时连接'; }
  else { d.className = 'dot off'; t.textContent = '已断开'; }
}

// ---------------- 主题 ----------------
function applyTheme(t) {
  document.documentElement.setAttribute('data-theme', t);
  $('themeBtn').textContent = t === 'dark' ? '☀' : '🌙';
  localStorage.setItem('sns_theme', t);
}

// ---------------- 启动 ----------------
function boot() {
  renderNav();
  route();
  connectWS();
}

window.addEventListener('hashchange', route);

document.addEventListener('DOMContentLoaded', () => {
  applyTheme(localStorage.getItem('sns_theme') ||
    (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'));
  $('themeBtn').onclick = () => applyTheme(document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark');
  $('refreshBtn').onclick = () => { route(); toast('已刷新'); };
  $('gateBtn').onclick = tryEnter;
  $('gateToken').onkeydown = (e) => { if (e.key === 'Enter') tryEnter(); };

  if (!S.token) { showGate(); return; }
  api('/api/admin/data').then(() => { $('gate').style.display = 'none'; boot(); })
    .catch(() => showGate('令牌已失效，请重新输入'));
});
