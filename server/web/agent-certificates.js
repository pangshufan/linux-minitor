'use strict';

const el = id => document.getElementById(id);
const endpoint = '/api/agent-certificates';

async function loadCertificateStatus() {
  try {
    const resp = await fetch(endpoint);
    if (resp.status === 401) { goToLogin(); return; }
    if (!resp.ok) throw Error('无法读取证书签发状态');
    const data = await resp.json();
    el('issue-fields').disabled = !data.enabled;
    el('token-note').hidden = !data.token_required;
    const certificates = data.certificates ?? [];
    el('certificate-info').hidden = certificates.length === 0;
    el('certificate-list').replaceChildren(...certificates.map(c => {
      const row = document.createElement('p');
      const days = Math.ceil((Date.parse(c.not_after) - Date.now()) / 86400000);
      row.textContent = c.usage + '：' + new Date(c.not_after).toLocaleString()
        + (days <= 0 ? '（已到期）' : '（剩余 ' + days + ' 天）');
      return row;
    }));
    if (!data.enabled) el('error').textContent = data.disabled_reason || '签发功能未启用，请检查 Server 的 HTTPS 和签发配置。';
  } catch (err) {
    el('error').textContent = err.message;
  }
}

el('valid-days').addEventListener('input', () => {
  el('validity-note').textContent = Number(el('valid-days').value) > 365
    ? '有效期超过一年，请考虑私钥泄露后的影响并妥善保管。允许继续签发，但不能超过签发 CA 的到期时间。'
    : '';
});

el('platform').addEventListener('change', () => {
  el('install-path').textContent = el('platform').value === 'windows'
    ? 'Windows：在 Agent 程序包内以管理员身份运行 install-agent.bat，按提示选择下载的 ZIP。'
    : 'Linux：在 Agent 程序包内执行 sudo bash install-agent.sh，按提示选择下载的 ZIP。';
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
    if (resp.status === 401) { goToLogin(); return; }
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
