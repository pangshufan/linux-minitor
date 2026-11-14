const element = id => document.getElementById(id);

function node(tag, text) {
  const el = document.createElement(tag);
  if (text !== undefined) el.textContent = text;
  return el;
}

function table(id, headings, rows) {
  const root = element(id);
  if (!rows.length) {
    root.replaceChildren(node('p', '暂无记录'));
    return;
  }
  const grid = node('table');
  const header = grid.createTHead().insertRow();
  for (const title of headings) header.append(node('th', title));
  const body = grid.createTBody();
  for (const values of rows) {
    const row = body.insertRow();
    for (const value of values) {
      const cell = row.insertCell();
      if (value instanceof Node) cell.append(value);
      else cell.textContent = value ?? '—';
    }
  }
  root.replaceChildren(grid);
}

const date = value => value ? new Date(value).toLocaleString() : '—';
const number = value => Number.isFinite(value) ? Number(value.toFixed(2)) : '—';
const states = {pending: '防抖中 / 待发送', firing: '触发中', sending: '发送中', delivered: '已送达', failed: '失败', suppressed: '已静默'};

async function api(path, body) {
  const response = await fetch(path, body === undefined ? undefined : {
    method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || response.statusText);
  return data;
}

function button(label, path, body) {
  const el = node('button', label);
  el.addEventListener('click', async () => {
    el.disabled = true;
    try {
      await api(path, body);
      await refresh();
    } catch (err) {
      element('error').textContent = err.message;
    } finally {
      el.disabled = false;
    }
  });
  return el;
}

let refreshID = 0;
async function refresh() {
  const id = ++refreshID;
  try {
    const [data, events] = await Promise.all([api('/api/alerts'), api('/api/alerts/events')]);
    if (id !== refreshID) return;
    element('error').textContent = '';
    element('alert-status').textContent = data.enabled ? '告警已启用' : '告警已停用';
    table('alert-active', ['规则', '主机', '对象', '状态', '观测值', '认领时间', '静默至', '操作'], data.active.map(item => {
      const actions = node('div');
      actions.className = 'actions';
      const path = '/api/alerts/' + encodeURIComponent(item.alert_id);
      if (item.state === 'firing') {
        if (!item.ack_ms) actions.append(button('认领', path + '/ack', {}));
        actions.append(button('静默 1 小时', path + '/mute', {seconds: 3600}));
      }
      actions.append(button('关闭', path + '/resolve', {}));
      return [item.rule_id, item.host_id, item.target, item.state === 'pending' ? '防抖中' : states[item.state],
        number(item.value), date(item.ack_ms), date(item.mute_until_ms), actions];
    }));
    table('alert-events', ['时间', '事件', '规则', '主机', '对象', '接收端投递状态'], events.map(event => {
      const deliveries = node('div');
      for (const task of event.deliveries) {
        const line = node('div');
        line.className = 'actions';
        line.append(node('span', [task.receiver, task.status === 'pending' ? '待发送' : states[task.status], task.last_error].filter(Boolean).join(' · ')));
        if (data.enabled && task.status === 'failed') line.append(button('重发', '/api/alerts/events/' + encodeURIComponent(event.event_id) + '/retry', {receiver: task.receiver}));
        deliveries.append(line);
      }
      return [date(event.occurred_at), [event.type, event.reason].filter(Boolean).join(' · '), event.rule_id, event.host_id, event.target, deliveries];
    }));
    table('alert-rules', ['规则', '开关', '阈值', '触发 / 恢复次数', 'CPU 窗口', '范围', '接收端'], data.rules.map(rule => [
      rule.id, rule.enabled ? '启用' : '停用', rule.threshold, rule.trigger_count + ' / ' + rule.recover_count,
      rule.metric === 'cpu' ? (rule.window_ms ? rule.window_ms + ' ms' : '基础窗口') : '—',
      [rule.host_id, rule.target].filter(Boolean).join(' · ') || '全部',
      rule.receivers === null ? '全部已配置接收端' : rule.receivers.join('、') || '仅站内',
    ]));
  } catch (err) {
    if (id === refreshID) element('error').textContent = err.message;
  }
}

element('alert-refresh').addEventListener('click', refresh);
refresh();
setInterval(refresh, 15000);
