// docker-forward WebUI：原生 ES Module，hash 路由，3s 轮询。

let token = localStorage.getItem('dr_token') || '';
let containers = []; // 缓存运行中容器，供 combobox 使用
let pollTimer = null;

// ---------- 基础设施 ----------

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));
}

async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  if (token) headers['Authorization'] = 'Bearer ' + token;
  if (opts.body) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, { ...opts, headers });
  if (res.status === 401) { showTokenPrompt(); throw new Error('未认证'); }
  if (res.status === 204) return null;
  let body = null;
  try { body = await res.json(); } catch { /* 空响应 */ }
  if (!res.ok) throw new Error(body?.error || `HTTP ${res.status}`);
  return body;
}

function toast(msg, isErr = true) {
  const el = document.createElement('div');
  el.className = 'toast' + (isErr ? ' err' : ' ok');
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

function showTokenPrompt() {
  const root = document.getElementById('modal-root');
  if (root.querySelector('.token-modal')) return;
  root.innerHTML = `
    <div class="modal-mask token-modal">
      <form class="modal card" id="token-form">
        <h3>需要访问令牌</h3>
        <p class="muted">服务端启用了 token 认证，请输入 Bearer Token：</p>
        <input type="password" id="token-input" placeholder="token" autocomplete="off" required>
        <div class="row end">
          <button type="submit" class="primary">保存</button>
        </div>
      </form>
    </div>`;
  document.getElementById('token-form').onsubmit = e => {
    e.preventDefault();
    token = document.getElementById('token-input').value.trim();
    localStorage.setItem('dr_token', token);
    root.innerHTML = '';
    route();
  };
}

// ---------- 路由 ----------

function route() {
  clearInterval(pollTimer);
  const hash = location.hash || '#/rules';
  document.querySelectorAll('nav a').forEach(a =>
    a.classList.toggle('active', hash.startsWith('#/' + a.dataset.route)));
  if (hash.startsWith('#/containers')) {
    renderContainers();
    pollTimer = setInterval(renderContainers, 3000);
  } else {
    renderRules();
    pollTimer = setInterval(renderRules, 3000);
  }
}

// ---------- 全局状态条 ----------

async function renderGlobalStatus() {
  try {
    const s = await api('/api/status');
    const el = document.getElementById('global-status');
    const dc = s.docker.connected ? '<span class="dot ok"></span>docker' : '<span class="dot bad"></span>docker';
    const ev = s.events.state === 'connected' ? '<span class="dot ok"></span>events' : '<span class="dot warn"></span>' + esc(s.events.state);
    el.innerHTML = `${dc} ${ev} <span class="muted">v${esc(s.version)}</span>`;
  } catch { /* token 弹窗已处理 */ }
}

// ---------- 规则列表 ----------

async function renderRules() {
  renderGlobalStatus();
  let data;
  try { data = await api('/api/rules'); } catch (e) { if (e.message !== '未认证') toast(e.message); return; }
  const rules = data.rules || [];
  const app = document.getElementById('app');
  const rows = rules.map(r => `
    <tr>
      <td>${esc(r.name || '-')}</td>
      <td><span class="tag">${esc(r.protocol)}</span></td>
      <td><code>${esc(r.listenAddr || '0.0.0.0')}:${r.listenPort}</code></td>
      <td>${esc(r.containerName)}:${r.targetPort}${r.network ? ` <span class="muted">@${esc(r.network)}</span>` : ''}</td>
      <td>${r.status.resolvedIp ? `<code>${esc(r.status.resolvedIp)}</code>` : '<span class="muted">-</span>'}</td>
      <td>${stateBadge(r.status)}</td>
      <td>${r.status.activeConns ?? 0}</td>
      <td class="ops">
        <button data-act="toggle" data-id="${esc(r.id)}">${r.enabled ? '停用' : '启用'}</button>
        <button data-act="edit" data-id="${esc(r.id)}">编辑</button>
        <button class="danger" data-act="del" data-id="${esc(r.id)}">删除</button>
      </td>
    </tr>`).join('');
  app.innerHTML = `
    <div class="bar">
      <h2>转发规则</h2>
      <button class="primary" id="btn-new">+ 新建规则</button>
    </div>
    ${rules.length ? `
    <table>
      <thead><tr>
        <th>名称</th><th>协议</th><th>监听</th><th>目标</th><th>解析 IP</th><th>状态</th><th>连接</th><th></th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table>` : '<p class="empty">还没有规则，点击「新建规则」开始。</p>'}`;
  document.getElementById('btn-new').onclick = () => openRuleForm(null);
  app.querySelectorAll('button[data-act]').forEach(b => b.onclick = () => onRuleAction(b.dataset.act, b.dataset.id, rules));
}

function stateBadge(st) {
  if (!st || !st.state) return '<span class="badge">-</span>';
  const cls = { running: 'ok', disabled: '', error: 'bad' }[st.state] ?? '';
  let extra = '';
  if (st.state === 'error' && st.lastError) extra = ` title="${esc(st.lastError)}"`;
  return `<span class="badge ${cls}"${extra}>${esc(st.state)}</span>`;
}

async function onRuleAction(act, id, rules) {
  const r = rules.find(x => x.id === id);
  if (!r) return;
  try {
    if (act === 'toggle') {
      await api(`/api/rules/${id}/${r.enabled ? 'disable' : 'enable'}`, { method: 'POST' });
    } else if (act === 'del') {
      if (!confirm(`删除规则「${r.name || r.id}」(${r.protocol}:${r.listenPort})？`)) return;
      await api(`/api/rules/${id}`, { method: 'DELETE' });
    } else if (act === 'edit') {
      openRuleForm(r);
      return;
    }
    renderRules();
  } catch (e) { toast(e.message); }
}

// ---------- 规则表单 ----------

async function openRuleForm(rule, prefill = {}) {
  const r = rule || {
    name: '', protocol: 'tcp', listenAddr: '0.0.0.0', listenPort: null,
    containerName: '', network: '', targetPort: null, enabled: true,
    closeOnSwitch: false, udpIdleTimeoutSec: 0, ...prefill,
  };
  try { containers = (await api('/api/containers')).containers || []; } catch { /* 容器列表失败不阻塞表单 */ }
  const root = document.getElementById('modal-root');
  root.innerHTML = `
    <div class="modal-mask">
      <form class="modal card wide" id="rule-form">
        <h3>${rule ? '编辑规则' : '新建规则'}</h3>
        <div class="grid">
          <label>名称<input name="name" value="${esc(r.name)}" placeholder="可选"></label>
          <label>协议
            <select name="protocol">
              <option value="tcp" ${r.protocol === 'tcp' ? 'selected' : ''}>TCP</option>
              <option value="udp" ${r.protocol === 'udp' ? 'selected' : ''}>UDP</option>
            </select>
          </label>
          <label>监听地址<input name="listenAddr" value="${esc(r.listenAddr || '0.0.0.0')}" required></label>
          <label>监听端口<input name="listenPort" type="number" min="1" max="65535" value="${r.listenPort ?? ''}" required></label>
          <label>目标容器
            <input name="containerName" list="dl-containers" value="${esc(r.containerName)}" placeholder="选择或输入容器名" required>
            <datalist id="dl-containers">
              ${containers.map(c => `<option value="${esc(c.name)}">${esc(c.image)} ${esc(Object.values(c.ips || {}).join(','))}</option>`).join('')}
            </datalist>
          </label>
          <label>目标端口
            <input name="targetPort" type="number" min="1" max="65535" list="dl-target-ports" value="${r.targetPort ?? ''}" placeholder="选择或输入" required>
            <datalist id="dl-target-ports"></datalist>
          </label>
          <label>网络 <span class="muted">(多网络时指定)</span>
            <input name="network" list="dl-networks" value="${esc(r.network || '')}" placeholder="默认自动">
            <datalist id="dl-networks"></datalist>
          </label>
          <label class="udp-only">UDP 空闲超时(秒)<input name="udpIdleTimeoutSec" type="number" min="0" value="${r.udpIdleTimeoutSec || 0}" placeholder="60"></label>
        </div>
        <div class="row">
          <label class="chk"><input type="checkbox" name="enabled" ${r.enabled ? 'checked' : ''}> 启用</label>
          <label class="chk"><input type="checkbox" name="closeOnSwitch" ${r.closeOnSwitch ? 'checked' : ''}> IP 切换时断开存量连接</label>
        </div>
        <p class="form-err" id="form-err"></p>
        <div class="row end">
          <button type="button" id="btn-cancel">取消</button>
          <button type="submit" class="primary">${rule ? '保存' : '创建'}</button>
        </div>
      </form>
    </div>`;

  const form = document.getElementById('rule-form');
  const protoSel = form.elements.protocol;
  const updateUdpVisibility = () => {
    form.querySelectorAll('.udp-only').forEach(el => el.style.display = protoSel.value === 'udp' ? '' : 'none');
  };
  updateUdpVisibility();

  // 选中容器后联动：网络候选 + 目标端口候选/预填（均按协议过滤）
  const refreshTargetPorts = () => {
    const c = containers.find(x => x.name === form.elements.containerName.value.trim());
    form.querySelector('#dl-target-ports').innerHTML = privatePorts(c, protoSel.value)
      .map(p => `<option value="${p.port}">${p.pub ? `宿主端口 ${p.pub}` : ''}</option>`).join('');
  };
  protoSel.onchange = () => { updateUdpVisibility(); refreshTargetPorts(); };
  form.elements.containerName.oninput = e => {
    const c = containers.find(x => x.name === e.target.value.trim());
    form.querySelector('#dl-networks').innerHTML =
      c ? Object.keys(c.ips || {}).map(n => `<option value="${esc(n)}">`).join('') : '';
    refreshTargetPorts();
    if (c && !rule && !form.elements.targetPort.value) {
      const p = firstPrivatePort(c, protoSel.value);
      if (p) form.elements.targetPort.value = p;
    }
  };
  refreshTargetPorts();

  document.getElementById('btn-cancel').onclick = () => root.innerHTML = '';
  form.onsubmit = async e => {
    e.preventDefault();
    const f = form.elements;
    const body = {
      name: f.name.value.trim(),
      protocol: f.protocol.value,
      listenAddr: f.listenAddr.value.trim() || '0.0.0.0',
      listenPort: +f.listenPort.value,
      containerName: f.containerName.value.trim(),
      network: f.network.value.trim(),
      targetPort: +f.targetPort.value,
      enabled: f.enabled.checked,
      closeOnSwitch: f.closeOnSwitch.checked,
      udpIdleTimeoutSec: +f.udpIdleTimeoutSec.value || 0,
    };
    try {
      if (rule) {
        await api(`/api/rules/${rule.id}`, { method: 'PUT', body: JSON.stringify(body) });
      } else {
        await api('/api/rules', { method: 'POST', body: JSON.stringify(body) });
      }
      root.innerHTML = '';
      toast('已保存', false);
      renderRules();
    } catch (err) {
      document.getElementById('form-err').textContent = err.message;
    }
  };
}

function firstPrivatePort(c, proto) {
  for (const p of c?.ports || []) {
    const m = p.match(/(?:->|^)(\d+)\/(tcp|udp)/);
    if (m && (!proto || m[2] === proto)) return m[1];
  }
  return '';
}

// privatePorts 提取容器侧端口候选，按协议过滤、按端口号去重，保持服务端排序
function privatePorts(c, proto) {
  const seen = new Set();
  const out = [];
  for (const p of c?.ports || []) {
    const m = p.match(/^(?:(\d+)->)?(\d+)\/(tcp|udp)$/);
    if (!m || seen.has(m[2]) || (proto && m[3] !== proto)) continue;
    seen.add(m[2]);
    out.push({ port: m[2], pub: m[1] });
  }
  return out;
}

// ---------- 容器总览 ----------

async function renderContainers() {
  renderGlobalStatus();
  let data;
  try { data = await api('/api/containers'); } catch (e) { if (e.message !== '未认证') toast(e.message); return; }
  containers = data.containers || [];
  const rows = containers.map(c => `
    <tr>
      <td>${esc(c.name)}</td>
      <td><code>${esc(c.id)}</code></td>
      <td class="img">${esc(c.image)}</td>
      <td>${Object.entries(c.ips || {}).map(([n, ip]) => `<code>${esc(ip)}</code> <span class="muted">@${esc(n)}</span>`).join('<br>') || '-'}</td>
      <td>${(c.ports || []).map(p => `<code>${esc(p)}</code>`).join('<br>') || '-'}</td>
      <td><button data-name="${esc(c.name)}">转发…</button></td>
    </tr>`).join('');
  document.getElementById('app').innerHTML = `
    <div class="bar"><h2>运行中容器</h2><span class="muted">${containers.length} 个，3s 自动刷新</span></div>
    ${containers.length ? `
    <table>
      <thead><tr><th>名称</th><th>ID</th><th>镜像</th><th>IP</th><th>端口</th><th></th></tr></thead>
      <tbody>${rows}</tbody>
    </table>` : '<p class="empty">没有运行中的容器。</p>'}`;
  document.querySelectorAll('button[data-name]').forEach(b =>
    b.onclick = () => {
      location.hash = '#/rules';
      const c = containers.find(x => x.name === b.dataset.name);
      openRuleForm(null, { containerName: b.dataset.name, targetPort: +firstPrivatePort(c) || null });
    });
}

// ---------- 启动 ----------

window.addEventListener('hashchange', route);
route();
