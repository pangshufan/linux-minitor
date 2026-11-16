'use strict';

const el = id => document.getElementById(id);
const endpoint = '/api/agent-certificates';

async function loadCertificateStatus() {
  try {
    const resp = await fetch(endpoint);
    if (!resp.ok) throw Error('无法读取证书签发状态');
    const data = await resp.json();
    el('issue-fields').disabled = !data.enabled;
    el('token-note').hidden = !data.token_required;
    if (!data.enabled) el('error').textContent = '签发功能未启用。请通过 HTTPS 访问，并在 Server 配置签发 CA。';
  } catch (err) {
    el('error').textContent = err.message;
  }
}

el('platform').addEventListener('change', () => {
  el('install-path').textContent = el('platform').value === 'windows'
    ? 'Windows：解压到 C:\\ProgramData\\Monitor，再用 Agent 安装包中的 PowerShell 脚本安装服务。'
    : 'Linux：将文件放到 /etc/monitor，设置 agent.key 权限为 0600，再安装或重启 Agent 服务。';
});

el('issue-form').addEventListener('submit', async event => {
  event.preventDefault();
  const fields = el('issue-fields');
  if (fields.disabled) return;
  const request = {
    host_id: el('host-id').value,
    platform: el('platform').value,
    server: el('grpc-address').value.trim(),
    valid_days: Number(el('valid-days').value),
  };
  fields.disabled = true;
  el('error').textContent = '';
  el('issue-status').textContent = '正在签发…';
  try {
    const resp = await fetch(endpoint, {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(request),
    });
    if (!resp.ok) {
      const result = await resp.json().catch(() => ({}));
      throw Error(result.error || '签发失败');
    }
    if (resp.headers.get('Content-Type') !== 'application/zip') throw Error('未收到证书安装包');
    const url = URL.createObjectURL(await resp.blob());
    const link = document.createElement('a');
    link.href = url;
    link.download = 'monitor-agent-' + request.host_id + '.zip';
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30_000);
    el('issue-status').textContent = '已签发并开始下载：' + request.host_id;
  } catch (err) {
    el('error').textContent = err.message;
    el('issue-status').textContent = '';
  } finally {
    fields.disabled = false;
  }
});

loadCertificateStatus();
