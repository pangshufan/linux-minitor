'use strict';

let dashboardSamples = [];
let dashboardSampleKey = '';
let dashboardIndexedPoints = null;
let dashboardHistoryKeys = new Set(), dashboardHistoryServices = new Map();
const dashboardColors = ['var(--chart-1)','var(--chart-2)','var(--chart-3)','var(--chart-4)'];
const scalar = value => Number.isFinite(value) ? value.toFixed(1) : '—';
const latency = value => Number.isFinite(value) ? value.toFixed(2) + ' ms' : '—';

function dashboardWindow(metrics) {
  const requested = document.getElementById('window').value;
  return requested ? metrics.windows?.find(w => w.requested_ms === Number(requested)) : metrics.windows?.[0];
}

function recordDashboardSample(metrics, filesystems, at) {
  if (!metrics?.collected_at) return;
  const w = dashboardWindow(metrics);
  const selectedProcess = document.getElementById('dashboard-process').value;
  const key = [metrics.collected_at, metrics.sample_id, w?.requested_ms, selectedProcess].join('/');
  if (key === dashboardSampleKey) return;
  dashboardSampleKey = key;
  const memory = metrics.memory_bytes ?? {};
  const rates = Object.fromEntries(Object.entries(w?.rates ?? {}).filter(([name]) => !name.startsWith('process/') || name === selectedProcess));
  dashboardSamples.push({
    received_ms: at, collected_at:metrics.collected_at, window_ms:w?.requested_ms,
    cpu_selection:w?.cpu_selection, cpu_time_percent:w?.cpu_percent?.cpu?.time_busy, cpu_utility_percent:w?.cpu_percent?.cpu?.utility,
    cpu_percent:w?.cpu_percent?.cpu?.busy, iowait_percent:w?.cpu_percent?.cpu?.iowait,
    memory_percent:ratio(memory.used,memory.total), swap_percent:ratio(memory.swap_used,memory.swap_total),
    rates, services:metrics.processes?.services ?? [],
    processes:(metrics.processes?.items ?? []).filter(p => p.key === selectedProcess), filesystems,
  });
  dashboardSamples = dashboardSamples.filter(p => p.received_ms >= Date.now() - 3600000).slice(-720);
}

function dashboardOptions(id, entries, placeholder) {
  const select = document.getElementById(id);
  const current = select.value;
  const options = new Map(entries);
  if (current && !options.has(current)) options.set(current, (select.selectedOptions[0]?.textContent ?? current).replace(/（已退出）$/, '') + '（已退出）');
  const signature = JSON.stringify([...options]);
  if (signature === select.dataset.options) return;
  select.dataset.options = signature;
  const rows = [...options].map(([value,label]) => new Option(label,value));
  if (placeholder) rows.unshift(new Option(placeholder,''));
  if (!rows.length) rows.push(new Option('未提供',''));
  select.replaceChildren(...rows);
  if (current && options.has(current)) select.value = current;
}

function syncDashboardOptions(metrics) {
  const rates = dashboardWindow(metrics)?.rates ?? {};
  if (dashboardIndexedPoints !== points) {
    dashboardIndexedPoints = points;
    dashboardHistoryKeys = new Set();
    dashboardHistoryServices = new Map();
    for (const point of points) {
      for (const key of Object.keys(point.rates ?? {})) dashboardHistoryKeys.add(key);
      for (const service of point.services ?? []) dashboardHistoryServices.set(service.key, service.name);
    }
  }
  const keys = new Set([...Object.keys(rates), ...dashboardHistoryKeys]);
  for (const [id,prefix] of [['dashboard-disk','disk/'],['dashboard-net','net/']]) {
    dashboardOptions(id, [...keys].filter(k => k.startsWith(prefix)).sort().map(k => [k,k.split('/').at(-1)]));
  }
  const services = new Map(dashboardHistoryServices);
  for (const s of metrics.processes?.services ?? []) services.set(s.key,s.name);
  dashboardOptions('dashboard-service', [...services]);
  dashboardOptions('dashboard-process',(metrics.processes?.items ?? []).map(p => [p.key,p.name + ' · PID ' + p.pid]),'选择进程查看趋势');
}

function dashboardData(metrics) {
  const window = dashboardWindow(metrics)?.requested_ms;
  const range = Number(document.getElementById('range').value.slice(0,-1)) * 3600000;
  const cutoff = Date.now() - range;
  const rows = new Map();
  for (const p of points) rows.set(p.collected_at || p.received_ms,p);
  for (const p of dashboardSamples) if (p.window_ms === window) rows.set(p.collected_at,p);
  return [...rows.values()].filter(p => p.received_ms >= cutoff).sort((a,b) => a.received_ms - b.received_ms);
}

function drawDashboardChart(title, series, samples, format, note) {
  const card = node('article', undefined, 'dashboard-chart');
  const heading = node('div', undefined, 'dashboard-chart-heading');
  heading.append(node('h3',title));
  card.append(heading);
  const legend = node('div',undefined,'chart-legend');
  series.forEach(([label,accessor],i) => {
    const item = node('span');
    const dot = node('i'); dot.style.backgroundColor = dashboardColors[i % dashboardColors.length];
    item.append(dot,document.createTextNode(label + ' ' + format(accessor(samples.at(-1) ?? {}))));
    legend.append(item);
  });
  card.append(legend);
  const visibleValues = samples.flatMap(p => series.map(([,f]) => f(p))).filter(Number.isFinite);
  if (!visibleValues.length) {
    card.append(node('p','暂无有效数据 · 等待采样或 Agent 未提供此指标','chart-empty'));
    if (note) card.append(node('p',note,'chart-note'));
    return card;
  }
  const create = (tag, attrs = {}, value) => {
    const element = document.createElementNS('http://www.w3.org/2000/svg',tag);
    for (const [k,v] of Object.entries(attrs)) element.setAttribute(k,v);
    if (value !== undefined) element.textContent = value;
    return element;
  };
  const width=500, height=160;
  const start=Date.now()-Number(document.getElementById('range').value.slice(0,-1))*3600000;
  const end=Date.now();
  const maximum=Math.max(format===percent ? 100 : 1,...visibleValues);
  const x=ms => 60 + (width-70)*(ms-start)/(end-start);
  const y=n => 130-110*n/maximum;
  const svg=create('svg',{viewBox:'0 0 500 160',role:'img','aria-label':title+' 动态趋势'});
  for (const fraction of [0,.5,1]) {
    svg.append(create('line',{x1:60,x2:490,y1:y(maximum*fraction),y2:y(maximum*fraction),class:'gridline'}));
    svg.append(create('text',{x:53,y:y(maximum*fraction)+4,'text-anchor':'end'},format(maximum*fraction)));
  }
  for (const [i,[label,accessor]] of series.entries()) {
    const sampleGap=Math.max(15000,1.8*(detail?.report_interval_ms||30000));
    const raw=samples.flatMap((p,j) => {
      const entry=[p.received_ms,accessor(p)];
      return j && p.received_ms-samples[j-1].received_ms>sampleGap ? [[p.received_ms,null],entry] : [entry];
    });
    // Preserve extrema, gaps and segment boundaries while bounding SVG complexity.
    const step=Math.max(1,Math.ceil(raw.length/180));
    const reduced=[];
    for (let n=0;n<raw.length;n+=step) {
      const chunk=raw.slice(n,n+step);
      if (chunk.some(([,v]) => !Number.isFinite(v))) { reduced.push(...chunk); continue; }
      let min=0,max=0;
      chunk.forEach(([,v],j) => {if(v<chunk[min][1])min=j;if(v>chunk[max][1])max=j;});
      for (const j of [...new Set([0,min,max,chunk.length-1])].sort((a,b)=>a-b)) reduced.push(chunk[j]);
    }
    let path='',previous=0;
    const gap=Math.max(15000,1.8*(detail?.report_interval_ms||30000));
    for (const [ms,value] of reduced) {
      if (!Number.isFinite(value)) {previous=0;continue;}
      const command=previous && ms-previous<=gap*step ? 'L' : 'M';
      if (command === 'M') svg.append(create('circle',{cx:x(ms),cy:y(value),r:1.8,fill:dashboardColors[i%dashboardColors.length]}));
      path+=command+x(ms).toFixed(1)+' '+y(value).toFixed(1)+' ';
      previous=ms;
    }
    const trace=create('path',{d:path,fill:'none',stroke:dashboardColors[i%dashboardColors.length],'stroke-width':2,'stroke-linejoin':'round'});
    trace.append(create('title',{},label));
    svg.append(trace);
  }
  for (const [t,anchor] of [[start,'start'],[end,'end']]) {
    svg.append(create('text',{x:x(t),y:153,'text-anchor':anchor},new Date(t).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'})));
  }
  card.append(svg);
  if (note) card.append(node('p',note,'chart-note'));
  return card;
}

function cpuMethodDescription(selection) {
  if (!selection) return '';
  const label = {time:'时间占用率',utility:'性能利用率'}[selection.actual] || '暂不可用';
  return label + ' · ' + (selection.source || '采集不可用') + ' · Windows ' + selection.windows_build +
    (selection.requested === 'auto' ? ' · 自动选择' : ' · 手动指定') +
    (selection.fallback ? ' · 已回退，首选计数器不可用' : '');
}

function renderDashboards() {
  if (!detail) return;
  const metrics = liveMetrics?.metrics ?? detail.metrics ?? {};
  syncDashboardOptions(metrics);
  const samples=dashboardData(metrics);
  const selection=dashboardWindow(metrics)?.cpu_selection;
  const isWindows=(liveInventory ?? detail.inventory)?.system?.id === 'windows' || !!selection;
  const selectedCPU=p => selection && p.cpu_selection?.actual !== selection.actual ? null : p.cpu_percent;
  const cpuSeries=isWindows ? [['主指标',selectedCPU],['时间占用',p=>p.cpu_time_percent],['性能利用',p=>p.cpu_utility_percent]] :
    [['CPU',p=>p.cpu_percent],['iowait',p=>p.iowait_percent]];
  const disk=document.getElementById('dashboard-disk').value;
  const net=document.getElementById('dashboard-net').value;
  const rate=(key,field) => p => p.rates?.[key]?.[field];
  const named=(key,fields) => fields.map(([label,field])=>[label,rate(key,field)]);
  const plot=(title,series,format,note) => drawDashboardChart(title,series,samples,format,note);
  const device=disk.split('/').at(-1)||'未提供';
  document.getElementById('dashboard-note').textContent =
    '在线时每 5 秒查询最新快照 · 采样窗口 ' + (dashboardWindow(metrics)?.requested_ms/1000 || '—') +
    ' 秒 · 历史上报周期 ' + (detail.report_interval_ms/1000 || '—') + ' 秒 · 指标采集于 ' + date(metrics.collected_at) +
    (detail.online ? '' : ' · 主机离线，展示历史及最后数据') + (selection ? ' · ' + cpuMethodDescription(selection) : '');
  document.getElementById('system-dashboard').replaceChildren(
    plot(isWindows ? 'CPU · Windows 统计口径' : 'CPU 与 I/O 等待',cpuSeries,percent,isWindows ? '主指标按版本及配置选择，展示范围 0–100%；原始性能利用率可超过 100%。Windows 未提供 Linux iowait。' : undefined),
    plot('内存与 Swap',[['内存',p=>p.memory_percent],['Swap',p=>p.swap_percent]],percent),
    plot('磁盘吞吐 · '+device,named(disk,[['读取','read_bytes'],['写入','write_bytes']]),byteRate),
    plot('磁盘忙碌时间 · '+device,named(disk,[['util','util_percent']]),percent),
    plot('磁盘 IOPS · '+device,named(disk,[['读 IOPS','reads'],['写 IOPS','writes']]),scalar),
    plot('磁盘平均延迟 · '+device,named(disk,[['读 await','read_await_ms'],['写 await','write_await_ms']]),latency,'无 I/O 完成时 await 不显示，避免把缺失当作 0 ms。'),
    plot('磁盘平均队列 · '+device,named(disk,[['平均队列','avg_queue_depth']]),scalar),
    plot('网络吞吐 · '+(net.split('/').at(-1)||'未提供'),named(net,[['接收','rx_bytes'],['发送','tx_bytes']]),byteRate),
    plot('网络错误 / 丢包 · '+(net.split('/').at(-1)||'未提供'),named(net,[['接收错误','rx_errors'],['发送错误','tx_errors'],['接收丢包','rx_drop'],['发送丢包','tx_drop']]),scalar,'单位：次/秒；数据缺失时不绘制。'),
  );
  const service=document.getElementById('dashboard-service').value;
  const serviceValue=field => p=>p.services?.find(s=>s.key===service)?.[field];
  const name=document.getElementById('dashboard-service').selectedOptions[0]?.textContent||'未配置服务';
  document.getElementById('service-dashboard').replaceChildren(
    plot(name+' · CPU',[['CPU',rate(service,'cpu_percent')]],percent,'服务 CPU 以单个逻辑核为 100%，多核服务可以超过 100%。'),
    plot(name+' · 内存',[['RSS / 工作集',serviceValue('rss_sum_bytes')],['cgroup',serviceValue('cgroup_memory_bytes')]],byte),
    plot(name+' · 磁盘 I/O',named(service,[['读取','read_bytes'],['写入','write_bytes']]),byteRate),
    plot(name+' · 进程数',[['进程数',serviceValue('process_count')]],scalar),
  );
  const process=document.getElementById('dashboard-process').value;
  const processValue=field=>p=>p.processes?.find(s=>s.key===process)?.[field];
  const processName=document.getElementById('dashboard-process').selectedOptions[0]?.textContent||'进程';
  document.getElementById('process-dashboard').replaceChildren(...(process ? [
    plot(processName+' · CPU',[['CPU',rate(process,'cpu_percent')]],percent,'按 PID 与启动标识跟踪；重启后的进程是新的监控对象。'),
    plot(processName+' · 内存',metrics.processes?.lifecycle_source === 'windows_process_snapshot' ? [['工作集',processValue('working_set_bytes')]] : [['RSS',processValue('rss_bytes')]],byte),
    plot(processName+' · 磁盘 I/O',named(process,[['读取','read_bytes'],['写入','write_bytes']]),byteRate),
  ] : [node('p','选择一个已监控进程，可查看该进程的 CPU、内存及磁盘 I/O 趋势。','muted')]));
}
