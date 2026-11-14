'use strict';

const el = id => document.getElementById(id);
const dash = '—';
const date = value => value && !Number.isNaN(new Date(value).getTime()) ? new Date(value).toLocaleString() : dash;
const percent = n => Number.isFinite(n) ? n.toFixed(1) + '%' : dash;
const ratio = (a, b) => Number.isFinite(a) && b > 0 ? 100 * a / b : null;
const statusText = {active: '采集正常', stalled: '采集停滞', warming_up: '等待采样', unknown: '采集状态未知'};
const byte = value => {
  if (value === null || value === '' || value === undefined) return dash;
  let n = Number(value);
  if (!Number.isFinite(n)) return dash;
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + ' ' + units[i];
};
const byteRate = value => Number.isFinite(value) ? byte(value) + '/s' : dash;

function node(tag, value, cls) {
  const e = document.createElement(tag);
  if (value !== undefined) e.textContent = value;
  if (cls) e.className = cls;
  return e;
}

function row(items) {
  const tr = node('tr');
  for (const item of items) {
    const td = node('td');
    if (item instanceof Node) td.append(item);
    else td.textContent = item ?? dash;
    tr.append(td);
  }
  return tr;
}

function table(target, labels, rows) {
  const root = el(target);
  if (!rows.length) {
    root.replaceChildren(node('p', '暂无数据', 'muted'));
    return;
  }
  const t = node('table'), head = node('thead'), columns = node('tr'), body = node('tbody');
  labels.forEach(label => columns.append(node('th', label)));
  rows.forEach(items => body.append(row(items)));
  head.append(columns);
  t.append(head, body);
  root.replaceChildren(t);
}

function badge(online, state) {
  if (!online) return node('span', '失联', 'badge offline');
  if (state === 'stalled') return node('span', '在线 · 采集停滞', 'badge stalled');
  return node('span', '在线', 'badge online');
}

let fleet = [], host = null, detail = null, liveMetrics = null, liveInventory = null, points = [];
let epoch = 0, historyRange = '', historyWindow = '', historyFetched = 0, nextRefresh;

async function api(path, options) {
  const response = await fetch(path, options);
  const result = await response.json();
  if (!response.ok) throw Error(result.error || '请求失败');
  return result;
}

function renderFleet() {
  const online = fleet.filter(h => h.online).length;
  el('fleet-summary').textContent = '共 ' + fleet.length + ' 台服务器 · ' + online + ' 台在线';
  const filter = el('filter').value;
  const rows = fleet.filter(h => filter === 'all' || h.online === (filter === 'online')).map(h => {
    const link = node('a', h.hostname);
    link.href = '#' + encodeURIComponent(h.host_id);
    const ident = node('div');
    ident.append(link, node('div', [h.os, h.architecture].filter(Boolean).join(' · '), 'muted'));
    return row([ident, badge(h.online, h.collection_status), percent(h.cpu_percent), percent(h.memory_percent),
      percent(h.root_percent), date(h.booted_at), date(h.last_seen_ms)]);
  });
  if (!rows.length) rows.push(row([fleet.length ? '没有符合条件的服务器' : '等待 Agent 接入']));
  el('fleet-rows').replaceChildren(...rows);
}

function factList(pairs) {
  return pairs.map(([label, value]) => {
    const d = node('div');
    d.append(node('dt', label), node('dd', value ?? dash));
    return d;
  });
}

function renderDetail() {
  if (!detail) return;
  el('detail').hidden = false;
  const inventory = liveInventory ?? detail.inventory ?? {};
  const system = inventory.system ?? {};
  const hardware = inventory.hardware ?? {};
  const metrics = liveMetrics?.metrics ?? detail.metrics ?? {};
  const memory = metrics.memory_bytes ?? {};
  const mounts = (liveMetrics?.filesystems ?? detail.filesystems)?.mounts ?? [];
  const windows = metrics.windows ?? [];
  const select = el('window');
  const selected = select.value;
  const options = windows.slice(1).map(w => String(w.requested_ms));
  const windowList = windows.map(w => w.requested_ms).join(',');
  if (windowList !== select.dataset.windows) {
    select.dataset.windows = windowList;
    const base = windows[0] ? '基础窗口（' + windows[0].requested_ms / 1000 + ' 秒）' : '基础窗口';
    select.replaceChildren(new Option(base, ''), ...options.map(value =>
      new Option(Number(value) / 1000 + ' 秒', value)));
    select.value = options.includes(selected) ? selected : '';
  }
  const primary = select.value ? windows.find(w => w.requested_ms === Number(select.value)) : windows[0];
  const cpu = primary?.cpu_percent?.cpu?.busy;
  const windowsHost = system.id === 'windows';
  const systemVolume = system.system_volume?.toLowerCase();
  const root = mounts.find(m => m.target === '/' || m.target?.toLowerCase() === systemVolume)?.capacity;
  el('host-title').textContent = system.hostname ?? detail.host_id;
  el('host-subtitle').replaceChildren(badge(detail.online, detail.collection_status),
    document.createTextNode('  ' + detail.host_id + ' · ' + (statusText[detail.collection_status] ?? '') + ' · 指标采集于 ' + date(metrics.collected_at)));
  const cards = [
    ['CPU', percent(cpu)], ['内存', percent(ratio(memory.used, memory.total))],
    ['Swap', percent(ratio(memory.swap_used, memory.swap_total))],
    [windowsHost ? '系统卷已用' : '根分区已用', percent(ratio(root?.used_bytes, root?.total_bytes))],
    ['运行时长', Number.isFinite(metrics.uptime_seconds) ? (metrics.uptime_seconds / 86400).toFixed(1) + ' 天' : dash]
  ];
  el('cards').replaceChildren(...cards.map(([label, value]) => {
    const card = node('div', undefined, 'card');
    card.append(node('span', label, 'muted'), node('strong', value));
    return card;
  }));
  el('system').replaceChildren(...factList([
    ['系统启动时间', date(metrics.booted_at ?? system.booted_at)], ['最后联系', date(detail.last_seen_ms)],
    ['本次 Agent 连接', date(detail.connected_ms)], ['最近配置采集', date(liveInventory?.collected_at ?? detail.inventory_ms)],
    ['发行版', system.pretty_name], ['内核', system.kernel_release], ['架构', system.architecture],
    ['厂商与机型', [hardware.dmi?.sys_vendor, hardware.dmi?.product_name].filter(Boolean).join(' ') || dash],
    ['BIOS / UEFI', hardware.dmi?.bios_version], ['整机序列号', hardware.dmi?.product_serial],
    ['CPU 型号', hardware.cpu?.model], ['物理插槽 / 核', (hardware.cpu?.packages_visible ?? dash) + ' / ' + (hardware.cpu?.cores_visible ?? dash)],
    ['逻辑 CPU', hardware.cpu?.logical_cpus_visible], ['系统内存总量', byte(hardware.os_memory_total_bytes ?? memory.total)]
  ]));

  table('rates', ['类型', '设备', '读取 / 接收', '写入 / 发送', '读 IOPS', '写 IOPS'],
    Object.entries(primary?.rates ?? {}).filter(([k]) => k.startsWith('disk/') || k.startsWith('net/')).sort().map(([k, v]) => {
      const disk = k.startsWith('disk/');
      return [disk ? '磁盘' : '网卡', k.split('/').at(-1), byteRate(v[disk ? 'read_bytes' : 'rx_bytes']),
        byteRate(v[disk ? 'write_bytes' : 'tx_bytes']), Number.isFinite(v.reads) ? v.reads.toFixed(1) : dash,
        Number.isFinite(v.writes) ? v.writes.toFixed(1) : dash];
    }));
  renderServices(metrics.processes, primary);
  table('mounts', ['挂载点', '来源', '格式', '模式', '容量', '已用', '可用', '采集状态'], mounts.map(m => [
    m.target, m.source, m.fstype, m.read_only ? '只读' : '读写', byte(m.capacity?.total_bytes),
    byte(m.capacity?.used_bytes), byte(m.capacity?.available_bytes), m.capacity_issue ?? date(m.capacity_at)
  ]));

  const devices = inventory.block_devices ?? {};
  table('disks', ['设备', '类别', '容量', '表格式', '分区类型', '内容', '未划分', '底层设备', '型号 / 序列号'],
    Object.entries(devices).sort(([,a],[,b]) => a.name.localeCompare(b.name)).map(([, d]) => [
      d.path, d.kind, byte(d.size_bytes), d.partition_table, d.partition_type,
      d.content_type, byte(d.unpartitioned?.bytes),
      d.parents?.map(id => devices[id]?.path ?? id).join('、'), [d.model, d.serial].filter(Boolean).join(' / ') || dash
    ]));
  const lvm = inventory.lvm ?? {};
  const lvmRows = [
    ...(lvm.physical_volumes ?? []).map(p => ['PV', p.pv_name, p.vg_name, byte(p.pv_size), byte(p.pv_free), p.pv_uuid]),
    ...(lvm.volume_groups ?? []).map(v => ['VG', v.vg_name, dash, byte(v.vg_size), byte(v.vg_free), v.pv_count + ' PV / ' + v.lv_count + ' LV']),
    ...(lvm.logical_volumes ?? []).map(l => {
      const members = (lvm.logical_volume_segments ?? []).filter(s => s.lv_uuid === l.lv_uuid);
      const paths = [...new Set(members.map(s => s.devices).filter(Boolean))].join('、');
      return ['LV', l.lv_path || l.lv_name, l.vg_name, byte(l.lv_size), dash, [l.lv_attr, paths].filter(Boolean).join(' · ')];
    })
  ];
  table('lvm', ['层级', '名称', '所属 VG', '容量', '剩余', '状态 / 底层'], lvmRows);
  table('hardware', ['类型', '名称', '配置', '附加信息'], [
    ...(hardware.memory_devices ?? []).map(d => ['DIMM', d.locator, [d.size, d.type, d.speed].filter(Boolean).join(' · '), d.manufacturer]),
    ...(hardware.network_interfaces ?? []).map(n => ['网卡', n.name, [n.mac, n.speed_mbps && n.speed_mbps + ' Mbps'].filter(Boolean).join(' · '), [n.driver, n.oper_state].filter(Boolean).join(' · ')])
  ]);
  el('live-metrics').disabled = el('live-inventory').disabled = !detail.online;
}

const expandedServices = new Map();
function renderServices(processes, primary) {
  const target = el('processes');
  el('process-note').textContent = !processes ? '该 Agent 未上报进程指标' : !processes.enabled ? '服务进程采集已关闭' :
    '已配置 ' + (processes.services?.length ?? 0) + ' 个服务 · ' + processes.reported + ' 个进程明细 · CPU 以单个逻辑核为 100%' +
    (processes.truncated ? ' · 明细已截取，部分汇总不可用' : '');
  if (!processes?.enabled) { target.replaceChildren(); return; }
  if (!processes.services) {
    table('processes', ['PID', '进程', 'CPU', 'RSS'], (processes.items ?? []).map(p =>
      [p.pid, p.name, percent(primary?.rates?.[p.key]?.cpu_percent), byte(p.rss_bytes)]));
    return;
  }
  if (!processes.services.length) { target.replaceChildren(node('p', '尚未配置监控服务，请在 Agent 的 process-services 中添加目标。', 'muted')); return; }
  const items = new Map((processes.items ?? []).map(p => [p.key, p]));
  const state = {running:'运行中', stopped:'已停止', degraded:'采集不完整 / 主进程异常', unknown:'状态未知'};
  const blocks = processes.services.map(s => {
    const d = node('details', undefined, 'service-block');
    d.open = expandedServices.get(s.key) ?? true;
    d.addEventListener('toggle', () => expandedServices.set(s.key, d.open));
    const summary = node('summary');
    summary.append(node('strong', s.name), document.createTextNode(' · ' + (state[s.state] ?? s.state) + ' · ' + s.process_count + ' 个进程 · ' + s.root_count + ' 个根进程'));
    const rates = primary?.rates?.[s.key] ?? {};
    const stats = node('dl', undefined, 'facts');
    stats.append(...factList([
      ['CPU', percent(rates.cpu_percent)], [s.metric_sources?.rss_sum === 'windows_working_set' ? '工作集合计' : 'RSS 合计', byte(s.rss_sum_bytes)],
      ['cgroup 内存', byte(s.cgroup_memory_bytes)], ['PSS 合计', byte(s.pss_sum_bytes)],
      ['线程 / FD 合计', (s.threads ?? dash) + ' / ' + (s.fd_sum ?? dash)],
      ['读取 / 写入', byteRate(rates.read_bytes) + ' / ' + byteRate(rates.write_bytes)],
      ['归属方式', s.membership_source + (s.cgroup ? ' · ' + s.cgroup : '')],
      ['指标来源', Object.entries(s.metric_sources ?? {}).map(([k,v]) => k + ': ' + v).join(' · ')],
      ['本次 Agent 运行观察到的重启', s.observed_restarts]
    ]));
    const members = new Set(s.process_keys ?? []), children = new Map(), roots = [];
    for (const key of members) {
      const p = items.get(key); if (!p) continue;
      if (p.parent_key && members.has(p.parent_key) && items.has(p.parent_key)) {
        if (!children.has(p.parent_key)) children.set(p.parent_key, []);
        children.get(p.parent_key).push(p);
      } else roots.push(p);
    }
    const t = node('table'), head = node('thead'), body = node('tbody');
    const labels = node('tr');
    for (const label of ['进程树 / PID', '状态', 'CPU', 'RSS / 工作集', '线程 / FD / 句柄', '累计读 / 写', '读取 / 写入', 'JVM Xmx', '启动时间']) labels.append(node('th', label));
    head.append(labels);
    const visited = new Set();
    const append = (first, initialDepth) => {
      const pending = [[first, initialDepth]];
      while (pending.length) {
        const [p, depth] = pending.pop();
        if (visited.has(p.key)) continue;
        visited.add(p.key);
        const r = primary?.rates?.[p.key] ?? {};
        const label = node('span', (depth ? '└─ ' : '') + p.name + ' · ' + p.pid, 'process-name');
        label.style.paddingInlineStart = Math.min(depth, 20) * 18 + 'px';
        body.append(row([label, p.state, percent(r.cpu_percent), byte(p.working_set_bytes ?? (processes.lifecycle_source === 'windows_process_snapshot' ? null : p.rss_bytes)), p.threads + ' / ' + (p.fd_count ?? dash) + ' / ' + (p.handle_count ?? dash),
          byte(p.io_counters?.read_bytes) + ' / ' + byte(p.io_counters?.write_bytes), byteRate(r.read_bytes) + ' / ' + byteRate(r.write_bytes),
          byte(p.jvm_max_heap_bytes), date(p.started_at)]));
        const descendants = (children.get(p.key) ?? []).slice().sort((a,b) => b.pid - a.pid);
        for (const child of descendants) pending.push([child, depth + 1]);
      }
    };
    roots.sort((a,b) => a.pid - b.pid).forEach(p => append(p, 0));
    // Defensive fallback for incomplete/cyclic parent data; never hide a reported process.
    for (const key of members) { const p = items.get(key); if (p && !visited.has(key)) append(p, 0); }
    t.append(head, body);
    const scroll = node('div', undefined, 'scroll'); scroll.append(t);
    d.append(summary, stats);
    if (Object.keys(s.issues ?? {}).length) d.append(node('p', Object.values(s.issues).join('；'), 'muted'));
    if (primary?.service_status?.[s.key]) {
      const unavailable = Object.entries(primary.service_status[s.key]).filter(([,v]) => v !== 'ok');
      if (unavailable.length) d.append(node('p', '速率暂不可用：' + unavailable.map(([k,v]) => k + ' (' + v + ')').join('、'), 'muted'));
    }
    if (!body.childNodes.length) d.append(node('p', s.state === 'stopped' ? '当前没有运行进程' : '暂无进程明细', 'muted'));
    else d.append(scroll);
    return d;
  });
  target.replaceChildren(...blocks);
}

function chartSeries() {
  const series = new Map([
    ['cpu', ['CPU %', p => p.cpu_percent, percent]],
    ['memory', ['内存 %', p => p.memory_percent, percent]],
    ['swap', ['Swap %', p => p.swap_percent, percent]]
  ]);
  for (const p of points) {
    for (const service of p.services ?? []) {
      const key = service.key, name = service.name;
      series.set(key + ':cpu', [name + ' CPU %', point => point.rates?.[key]?.cpu_percent, percent]);
      series.set(key + ':rss', [name + (service.metric_sources?.rss_sum === 'windows_working_set' ? ' 工作集合计' : ' RSS 合计'), point => point.services?.find(s => s.key === key)?.rss_sum_bytes, byte]);
      for (const field of ['read_bytes', 'write_bytes']) series.set(key + ':' + field,
        [name + (field === 'read_bytes' ? ' 读取' : ' 写入'), point => point.rates?.[key]?.[field], byteRate]);
    }
    for (const k of Object.keys(p.rates ?? {})) {
      const name = k.split('/').at(-1);
      if (!k.startsWith('disk/') && !k.startsWith('net/')) continue;
      const metrics = k.startsWith('disk/') ? [['read_bytes', '读'], ['write_bytes', '写']] : [['rx_bytes', '接收'], ['tx_bytes', '发送']];
      for (const [field, label] of metrics) {
        series.set(k + ':' + field, [name + ' ' + label, point => point.rates?.[k]?.[field], byteRate]);
      }
    }
    for (const m of p.filesystems?.mounts ?? []) {
      if (m.capacity) series.set('mount:' + m.target, [m.target + ' 已用 %', point => {
        const fs = point.filesystems?.mounts?.find(entry => entry.target === m.target)?.capacity;
        return ratio(fs?.used_bytes, fs?.total_bytes);
      }, percent]);
    }
  }
  return series;
}

function renderChart() {
  const series = chartSeries(), select = el('series'), previous = select.value;
  select.replaceChildren(...Array.from(series, ([key, [name]]) => {
    const item = node('option', name);
    item.value = key;
    return item;
  }));
  select.value = series.has(previous) ? previous : 'cpu';
  const [label, accessor, format] = series.get(select.value);
  const result = points.map(p => [p.received_ms, accessor(p)]);
  const values = result.filter(([,n]) => Number.isFinite(n));
  el('history-note').textContent = points.length ? points.length + ' 个上报点 · 采集窗口 ' + (points.at(-1).window_ms || dash) + ' ms · 横轴为中心端接收时间' : '暂无历史数据';
  if (!values.length) {
    el('chart').replaceChildren(node('p', '所选指标暂无历史点', 'muted'));
    return;
  }
  const svgNode = (tag, attrs, value) => {
    const e = document.createElementNS('http://www.w3.org/2000/svg', tag);
    for (const [key, val] of Object.entries(attrs)) e.setAttribute(key, val);
    if (value !== undefined) e.textContent = value;
    return e;
  };
  const svg = svgNode('svg', {viewBox: '0 0 900 230', role: 'img', 'aria-label': label + ' 历史曲线'});
  const end = Date.now(), start = end - Number(el('range').value.slice(0, -1)) * 3600000;
  const max = Math.max(format === percent ? 100 : 1, ...values.map(([,n]) => n));
  const x = ms => 110 + 765 * (ms - start) / (end - start);
  const y = n => 190 - 170 * n / max;
  for (let i = 0; i <= 4; i++) {
    const v = max * i / 4;
    svg.append(svgNode('line', {x1:110, x2:875, y1:y(v), y2:y(v), class:'gridline'}),
      svgNode('text', {x:104, y:y(v) + 4, 'text-anchor':'end'}, format(v)));
  }
  for (let i = 0; i <= 4; i++) {
    const t = start + (end - start) * i / 4;
    svg.append(svgNode('text', {x:x(t), y:215, 'text-anchor':'middle'}, new Date(t).toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'})));
  }
  let path = '', last = 0;
  const gap = 1.8 * (detail?.report_interval_ms || 30000);
  for (const [ms, value] of result) {
    if (!Number.isFinite(value) || ms < start || ms > end) { last = 0; continue; }
    const command = last && ms - last <= gap ? 'L' : 'M';
    path += command + x(ms).toFixed(2) + ' ' + y(value).toFixed(2) + ' ';
    if (command === 'M') svg.append(svgNode('circle', {cx:x(ms), cy:y(value), r:2.7, class:'point'}));
    last = ms;
  }
  svg.append(svgNode('path', {d:path, class:'trace'}));
  el('chart').replaceChildren(svg);
}

async function refresh(revision) {
  clearTimeout(nextRefresh);
  const id = host;
  try {
    if (!id) {
      const data = await api('/api/hosts');
      if (revision !== epoch) return;
      fleet = data;
      renderFleet();
    } else {
      const endpoint = '/api/hosts/' + encodeURIComponent(id);
      const data = await api(endpoint);
      if (revision !== epoch) return;
      detail = data;
      if (liveMetrics && Date.parse(data.metrics?.collected_at) >= Date.parse(liveMetrics.metrics?.collected_at)) liveMetrics = null;
      if (liveInventory && data.inventory_ms >= Date.parse(liveInventory.collected_at)) liveInventory = null;
      renderDetail();
      const range = el('range').value;
      const windowMS = el('window').value;
      const sameQuery = range === historyRange && windowMS === historyWindow;
      if (!sameQuery || Date.now() - historyFetched >= 30000) {
        if (!sameQuery) {
          points = [];
          el('chart').replaceChildren();
          el('history-note').textContent = '正在加载历史数据…';
        }
        const cursor = sameQuery ? points.at(-1)?.received_ms : undefined;
        const params = new URLSearchParams({range});
        if (windowMS) params.set('window_ms', windowMS);
        if (cursor !== undefined) params.set('since_ms', cursor);
        const history = await api(endpoint + '/history?' + params);
        if (revision !== epoch) return;
        // Include the cursor timestamp again so samples tied at the boundary are replaced together.
        const cutoff = Date.now() - Number(range.slice(0, -1)) * 3600000;
        const previous = cursor === undefined ? [] : points.filter(p => p.received_ms < cursor);
        points = previous.concat(history).filter(p => p.received_ms >= cutoff).slice(-3000);
        historyFetched = Date.now();
        historyRange = range;
        historyWindow = windowMS;
        renderChart();
      }
    }
    el('error').textContent = '';
  } catch (err) {
    if (revision === epoch) el('error').textContent = err.message;
  } finally {
    if (revision === epoch) nextRefresh = setTimeout(() => refresh(revision), 5000);
  }
}

async function live(kind) {
  const id = host, revision = epoch;
  el('live-message').textContent = '正在查询…';
  try {
    const result = await api('/api/hosts/' + encodeURIComponent(id) + '/query/' + kind, {
      method:'POST', headers:{'Content-Type':'application/json'}
    });
    if (revision !== epoch) return;
    if (kind === 'latest') liveMetrics = result;
    else liveInventory = result.inventory;
    renderDetail();
    el('live-message').textContent = '已获取 Agent 快照';
  } catch (err) {
    if (revision === epoch) el('live-message').textContent = err.message;
  }
}

function route() {
  host = window.location.hash.slice(1) || null;
  if (host) {
    try { host = decodeURIComponent(host); } catch { host = null; }
  }
  el('fleet').hidden = Boolean(host);
  el('detail').hidden = true;
  el('live-message').textContent = '';
  detail = liveMetrics = liveInventory = null;
  points = [];
  el('chart').replaceChildren();
  el('history-note').textContent = '';
  historyRange = '';
  historyWindow = '';
  el('window').value = '';
  historyFetched = 0;
  refresh(++epoch);
}

el('filter').addEventListener('change', renderFleet);
el('range').addEventListener('change', () => refresh(++epoch));
el('window').addEventListener('change', () => {
  renderDetail();
  refresh(++epoch);
});
el('series').addEventListener('change', renderChart);
el('live-inventory').addEventListener('click', () => live('inventory'));
el('live-metrics').addEventListener('click', () => live('latest'));
window.addEventListener('hashchange', route);
route();
