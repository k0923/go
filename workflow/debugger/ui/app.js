'use strict'

const $ = (sel, root = document) => root.querySelector(sel)
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)]

const state = {
  runs: [],
  current: null,
  detail: null,
  events: [],
  lastEventId: 0,
  tab: 'steps',
  search: '',
  statusFilter: 'ALL',
  eventFilter: 'ALL',
  drafts: {},
  drawerKey: null,
}

// ---------- formatting ----------

const STATUS = {
  COMPLETED: { label: 'Completed', chip: 'bg-emerald-500/10 text-emerald-300 ring-1 ring-emerald-500/25', dot: 'bg-emerald-400', bar: 'bg-emerald-500' },
  RUNNING: { label: 'Running', chip: 'bg-sky-500/10 text-sky-300 ring-1 ring-sky-500/25', dot: 'bg-sky-400 animate-pulse', bar: 'bg-sky-500' },
  SUSPENDED: { label: 'Suspended', chip: 'bg-amber-500/10 text-amber-300 ring-1 ring-amber-500/25', dot: 'bg-amber-400', bar: 'bg-amber-500' },
  FAILED: { label: 'Failed', chip: 'bg-rose-500/10 text-rose-300 ring-1 ring-rose-500/25', dot: 'bg-rose-400', bar: 'bg-rose-500' },
  CANCELLED: { label: 'Cancelled', chip: 'bg-slate-500/10 text-slate-300 ring-1 ring-slate-500/25', dot: 'bg-slate-400', bar: 'bg-slate-500' },
  RETRYING: { label: 'Retrying', chip: 'bg-orange-500/10 text-orange-300 ring-1 ring-orange-500/25', dot: 'bg-orange-400 animate-pulse', bar: 'bg-orange-500' },
  CREATED: { label: 'Created', chip: 'bg-slate-500/10 text-slate-300 ring-1 ring-slate-500/25', dot: 'bg-slate-400', bar: 'bg-slate-500' },
}
const st = s => STATUS[s] || STATUS.CREATED

const KIND = {
  ACTION: { label: 'Action', cls: 'text-indigo-300 bg-indigo-500/10 ring-indigo-500/20', icon: '<path d="M13 2 4 14h7l-1 8 9-12h-7z"/>' },
  SIGNAL: { label: 'WaitFor', cls: 'text-fuchsia-300 bg-fuchsia-500/10 ring-fuchsia-500/20', icon: '<path d="M4 4h16v12H5.2L4 17.2z"/><path d="M8 9h8M8 12h5"/>' },
  SLEEP: { label: 'Sleep', cls: 'text-cyan-300 bg-cyan-500/10 ring-cyan-500/20', icon: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>' },
  CHECKPOINT: { label: 'Checkpoint', cls: 'text-teal-300 bg-teal-500/10 ring-teal-500/20', icon: '<path d="M5 3v18M5 4h11l-2 4 2 4H5"/>' },
  DEFINITION: { label: 'Definition', cls: 'text-slate-300 bg-slate-500/10 ring-slate-500/20', icon: '<path d="M7 3h7l5 5v13H7z"/><path d="M14 3v5h5"/>' },
}
const kd = k => KIND[k] || KIND.DEFINITION

// Live refreshes re-render often; only touch the DOM when markup changed so
// clicks, hover and text selection survive an unchanged refresh.
const lastHTML = new WeakMap()
function setHTML(sel, html) {
  const el = $(sel)
  if (lastHTML.get(el) === html) return
  lastHTML.set(el, html)
  el.innerHTML = html
}

const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
const isZero = t => !t || t.startsWith('0001-01-01')

function fmtTime(t) {
  if (isZero(t)) return '—'
  const d = new Date(t)
  return d.toLocaleTimeString('zh-CN', { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0')
}
function fmtDate(t) {
  if (isZero(t)) return '—'
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}
function fmtDur(ms) {
  if (ms == null || isNaN(ms)) return '—'
  if (ms < 0) ms = 0
  if (ms < 1) return '<1ms'
  if (ms < 1000) return Math.round(ms) + 'ms'
  if (ms < 60000) return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + 's'
  const m = Math.floor(ms / 60000), s = Math.round((ms % 60000) / 1000)
  if (m < 60) return `${m}m ${s}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}
function ago(t) {
  if (isZero(t)) return '—'
  const s = (Date.now() - new Date(t)) / 1000
  if (s < 5) return '刚刚'
  if (s < 60) return Math.floor(s) + ' 秒前'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前'
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前'
  return Math.floor(s / 86400) + ' 天前'
}
function countdown(t) {
  if (isZero(t)) return ''
  const ms = new Date(t) - Date.now()
  return ms <= 0 ? '已到期' : '剩余 ' + fmtDur(ms)
}

// Payloads arrive as inline JSON (json.RawMessage); absent means undefined.
const decodeRaw = raw => raw ?? undefined

function highlight(value) {
  if (value === undefined) return '<span class="json-null">—</span>'
  const json = JSON.stringify(value, null, 2)
  if (json === undefined) return '<span class="json-null">—</span>'
  return esc(json).replace(/(&quot;(?:\\u[a-fA-F0-9]{4}|\\[^u]|[^\\&]|&(?!quot;))*?&quot;)(\s*:)?|\b(true|false)\b|\bnull\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g,
    (m, str, colon, bool) => {
      if (str) return colon ? `<span class="json-key">${str}</span>${colon}` : `<span class="json-str">${str}</span>`
      if (bool) return `<span class="json-bool">${m}</span>`
      if (m === 'null') return `<span class="json-null">${m}</span>`
      return `<span class="json-num">${m}</span>`
    })
}

function jsonBlock(value, extra = '') {
  return `<pre class="text-xs font-mono leading-relaxed bg-slate-950/70 ring-1 ring-white/5 rounded-lg p-3 overflow-x-auto scroll-thin ${extra}">${highlight(value)}</pre>`
}

function chip(status) {
  const s = st(status)
  return `<span class="chip ${s.chip}"><span class="h-1.5 w-1.5 rounded-full ${s.dot}"></span>${s.label}</span>`
}

function kindBadge(kind) {
  const k = kd(kind)
  return `<span class="inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium ring-1 ${k.cls}">
    <svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${k.icon}</svg>${k.label}</span>`
}

function toast(message, kind = 'ok') {
  const el = document.createElement('div')
  const cls = kind === 'ok' ? 'ring-emerald-500/30 text-emerald-200' : kind === 'warn' ? 'ring-amber-500/30 text-amber-200' : 'ring-rose-500/30 text-rose-200'
  el.className = `fade-up max-w-sm rounded-lg bg-slate-900/95 backdrop-blur px-4 py-3 text-sm shadow-xl ring-1 ${cls}`
  el.textContent = message
  $('#toasts').appendChild(el)
  setTimeout(() => { el.style.transition = 'opacity .3s'; el.style.opacity = '0'; setTimeout(() => el.remove(), 300) }, 3200)
}

// ---------- API ----------

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  })
  const text = await res.text()
  const data = text ? JSON.parse(text) : null
  if (!res.ok) throw new Error(data?.message || `HTTP ${res.status}`)
  return data
}

const runPath = (id, suffix = '') => `/api/runs/${encodeURIComponent(id)}${suffix}`

// ---------- runs list ----------

async function loadRuns() {
  state.runs = await api('/api/runs')
  renderRuns()
}

function renderFilters() {
  const counts = { ALL: state.runs.length }
  for (const r of state.runs) counts[r.status] = (counts[r.status] || 0) + 1
  const items = ['ALL', 'RUNNING', 'SUSPENDED', 'RETRYING', 'COMPLETED', 'FAILED'].filter(s => s === 'ALL' || counts[s])
  setHTML('#status-filter', items.map(s => {
    const active = state.statusFilter === s
    return `<button data-status="${s}" class="rounded-full px-2.5 py-1 ring-1 transition ${active ? 'bg-indigo-500/20 text-indigo-200 ring-indigo-500/40' : 'text-slate-400 ring-white/5 hover:text-slate-200'}">
      ${s === 'ALL' ? '全部' : st(s).label} <span class="opacity-60">${counts[s] || 0}</span></button>`
  }).join(''))
}

function renderRuns() {
  renderFilters()
  const q = state.search.toLowerCase()
  const runs = state.runs.filter(r =>
    (state.statusFilter === 'ALL' || r.status === state.statusFilter) &&
    (!q || r.id.toLowerCase().includes(q) || r.workflow.toLowerCase().includes(q)))
  if (!runs.length) {
    setHTML('#run-list', `<div class="text-center text-sm text-slate-500 py-12">${state.runs.length ? '没有匹配的 Run' : '还没有 Run<br><span class="text-xs">启动你的工作流后会自动出现</span>'}</div>`)
    return
  }
  const groups = new Map()
  for (const r of runs) {
    const key = r.workflow || '(unnamed)'
    if (!groups.has(key)) groups.set(key, [])
    groups.get(key).push(r)
  }
  setHTML('#run-list', [...groups].map(([wf, list]) => `
    <div>
      <div class="px-2 mb-1.5 flex items-center justify-between text-[11px] uppercase tracking-wider text-slate-500 font-semibold">
        <span class="truncate">${esc(wf)}</span><span>${list.length}</span>
      </div>
      <div class="space-y-1">${list.map(runItem).join('')}</div>
    </div>`).join(''))
}

function runItem(r) {
  const active = r.id === state.current
  const p = r.progress
  const pct = p.total ? Math.round((p.completed / p.total) * 100) : 0
  return `<a href="#/runs/${encodeURIComponent(r.id)}" class="block rounded-xl px-3 py-2.5 transition ring-1 ${active ? 'bg-indigo-500/10 ring-indigo-500/30' : 'ring-transparent hover:bg-white/[.03]'}">
    <div class="flex items-center justify-between gap-2">
      <div class="flex items-center gap-2 min-w-0">
        <span class="h-2 w-2 rounded-full shrink-0 ${st(r.status).dot}"></span>
        <span class="font-mono text-[13px] truncate ${active ? 'text-white' : 'text-slate-300'}">${esc(r.id)}</span>
      </div>
      ${r.online ? '<span class="text-[10px] text-emerald-400 shrink-0" title="worker 在线">● live</span>' : ''}
    </div>
    <div class="mt-2 flex items-center gap-2">
      <div class="flex-1 h-1 rounded-full bg-slate-800 overflow-hidden"><div class="h-full ${st(r.status).bar}" style="width:${pct}%"></div></div>
      <span class="text-[11px] text-slate-500 tabular-nums">${p.completed}/${p.total}</span>
    </div>
    <div class="mt-1.5 flex items-center justify-between text-[11px] text-slate-500">
      <span>${st(r.status).label}${r.pending?.length ? ` · ${r.pending.length} 等待` : ''}</span>
      <span>${ago(r.updated_at)}</span>
    </div>
  </a>`
}

// ---------- detail ----------

async function selectRun(id) {
  if (state.current !== id) {
    state.current = id
    state.detail = null
    state.events = []
    state.lastEventId = 0
    state.drafts = {}
    state.waitSignature = null
  }
  $('#empty').classList.toggle('hidden', !!id)
  $('#detail').classList.toggle('hidden', !id)
  renderRuns()
  if (!id) return
  await refreshDetail()
}

async function refreshDetail() {
  const id = state.current
  if (!id) return
  try {
    const [detail, events] = await Promise.all([
      api(runPath(id)),
      api(runPath(id, `/events?after=${state.lastEventId}&limit=2000`)),
    ])
    if (id !== state.current) return
    state.detail = detail
    if (events.length) {
      state.events.push(...events)
      state.lastEventId = events[events.length - 1].id
    }
    renderDetail()
  } catch (err) {
    if (/not found/i.test(err.message)) {
      location.hash = ''
      toast('Run 已被删除', 'warn')
    } else {
      toast(err.message, 'err')
    }
  }
}

function renderDetail() {
  const { run, records } = state.detail
  $('#d-title').textContent = run.id
  setHTML('#d-status', chip(run.status))
  setHTML('#d-online', run.online
    ? '<span class="chip bg-emerald-500/10 text-emerald-300 ring-1 ring-emerald-500/20"><span class="h-1.5 w-1.5 rounded-full bg-emerald-400 animate-pulse"></span>worker 在线</span>'
    : `<span class="chip bg-slate-500/10 text-slate-400 ring-1 ring-white/5">worker 离线${isZero(run.last_seen) ? '' : ' · ' + ago(run.last_seen)}</span>`)
  setHTML('#d-meta', [
    ['Workflow', `<span class="text-slate-200">${esc(run.workflow || '—')}</span>`],
    ['Version', `<span class="font-mono text-slate-200">${esc(run.version || '—')}</span>`],
    ['Drives', `<span class="text-slate-200 tabular-nums">${run.drives}</span>`],
    ['创建', fmtDate(run.created_at)],
    ['更新', ago(run.updated_at)],
  ].map(([k, v]) => `<span><span class="text-slate-500">${k}</span> ${v}</span>`).join(''))

  const p = run.progress
  const stats = [
    ['步骤', p.total, 'text-white'],
    ['已完成', p.completed, 'text-emerald-300'],
    ['执行中', p.running, 'text-sky-300'],
    ['等待中', p.suspended, 'text-amber-300'],
    ['失败 / 取消', `${p.failed} / ${p.cancelled}`, 'text-rose-300'],
  ]
  setHTML('#d-stats', stats.map(([k, v, c]) => `
    <div class="rounded-xl bg-slate-900/70 ring-1 ring-white/5 px-4 py-3">
      <div class="text-[11px] text-slate-500 uppercase tracking-wider">${k}</div>
      <div class="mt-1 text-xl font-semibold tabular-nums ${c}">${v}</div>
    </div>`).join(''))
  const total = p.total || 1
  setHTML('#d-bar', [['completed', 'bg-emerald-500'], ['running', 'bg-sky-500'], ['suspended', 'bg-amber-500'], ['failed', 'bg-rose-500'], ['cancelled', 'bg-slate-500']]
    .map(([k, c]) => p[k] ? `<div class="${c} transition-all duration-500" style="width:${(p[k] / total) * 100}%"></div>` : '').join(''))

  const err = $('#d-error')
  err.classList.toggle('hidden', !run.error)
  err.textContent = run.error || ''

  renderSteps(records)
  renderEvents()
  renderMessages()
  renderIO()
  renderWaits()
  if (state.drawerKey) openDrawer(state.drawerKey, true)
}

// Steps grouped by key prefix (RunFlow / Loop / WaitAny namespaces).
function renderSteps(records) {
  const steps = records.filter(r => r.kind !== 'DEFINITION')
  if (!steps.length) {
    setHTML('#p-steps', '<div class="text-sm text-slate-500 py-10 text-center">暂无步骤记录</div>')
    return
  }
  const now = Date.now()
  const starts = steps.map(r => new Date(r.started_at).getTime()).filter(t => t > 0)
  const t0 = Math.min(...starts)
  const t1 = Math.max(now, ...steps.map(r => isZero(r.ended_at) ? now : new Date(r.ended_at).getTime()))
  const span = Math.max(t1 - t0, 1)

  setHTML('#p-steps', `
    <div class="rounded-xl ring-1 ring-white/5 overflow-hidden">
      <div class="grid grid-cols-[minmax(0,2.2fr)_110px_120px_minmax(0,1.6fr)_80px] gap-4 px-4 py-2.5 text-[11px] uppercase tracking-wider text-slate-500 bg-slate-900/70 border-b border-white/5">
        <div>Key</div><div>类型</div><div>状态</div><div>时间线</div><div class="text-right">耗时</div>
      </div>
      ${steps.map(r => stepRow(r, t0, span, now)).join('')}
    </div>`)
}

function stepRow(r, t0, span, now) {
  const parts = r.key.split('|')
  const depth = parts.length - 1
  const start = new Date(r.started_at).getTime()
  const end = isZero(r.ended_at) ? now : new Date(r.ended_at).getTime()
  const left = start > 0 ? ((start - t0) / span) * 100 : 0
  const width = start > 0 ? Math.max(((end - start) / span) * 100, 0.8) : 0
  const s = st(r.status)
  let extra = ''
  if (r.kind === 'SLEEP' && r.status === 'SUSPENDED') extra = `<span class="text-cyan-300/80">${countdown(r.until)}</span>`
  if (r.group) extra = `<span class="text-slate-500">group ${esc(r.group.split('|').pop())}</span>`
  if (r.error) extra = `<span class="text-rose-300/90 truncate">${esc(r.error)}</span>`
  return `<button data-record="${esc(r.key)}" class="w-full text-left grid grid-cols-[minmax(0,2.2fr)_110px_120px_minmax(0,1.6fr)_80px] gap-4 px-4 py-2.5 items-center border-b border-white/[.03] last:border-0 hover:bg-white/[.03] transition">
    <div class="min-w-0" style="padding-left:${depth * 16}px">
      <div class="font-mono text-[13px] truncate">
        ${depth ? `<span class="text-slate-600">${esc(parts.slice(0, -1).join(' › '))} › </span>` : ''}<span class="text-slate-100">${esc(parts[parts.length - 1])}</span>
      </div>
      ${extra ? `<div class="text-[11px] mt-0.5 truncate">${extra}</div>` : ''}
    </div>
    <div>${kindBadge(r.kind)}</div>
    <div>${chip(r.status)}</div>
    <div class="relative h-2 rounded-full bg-slate-800/80">
      <div class="absolute top-0 h-2 rounded-full ${s.bar} ${r.status === 'RUNNING' || r.status === 'SUSPENDED' ? 'opacity-70' : ''}" style="left:${left}%;width:${width}%"></div>
    </div>
    <div class="text-right text-xs text-slate-400 tabular-nums">${start > 0 ? fmtDur(end - start) : '—'}</div>
  </button>`
}

// ---------- events ----------

const EVENT_STYLE = {
  drive_started: ['bg-slate-400', 'Drive 开始'],
  drive_finished: ['bg-slate-400', 'Drive 结束'],
  action_started: ['bg-sky-400', 'Action 开始'],
  action_finished: ['bg-indigo-400', 'Action 结束'],
  action_replayed: ['bg-slate-500', 'Action 回放'],
  wait_suspended: ['bg-amber-400', '挂起等待'],
  wait_resolved: ['bg-emerald-400', '等待已解决'],
  'store.put': ['bg-teal-400', '写入记录'],
  'store.commit': ['bg-indigo-400', '提交 Action'],
  'store.resolve': ['bg-emerald-400', '信号解决'],
  'store.cancel': ['bg-slate-400', '取消成员'],
  'store.wait_any': ['bg-fuchsia-400', '注册 WaitAny'],
  'store.wait_any_settled': ['bg-emerald-400', 'WaitAny 胜出'],
  'store.fail_wait': ['bg-rose-400', '成员失败'],
  'debugger.submit': ['bg-fuchsia-400', '提交 JSON'],
  'debugger.fire_sleep': ['bg-cyan-400', '立即唤醒 Sleep'],
  'debugger.redrive': ['bg-violet-400', '手动 Re-drive'],
  'run.attached': ['bg-violet-400', 'Worker 接入'],
}

const EVENT_FILTERS = [
  ['ALL', '全部'],
  ['engine', '引擎事件'],
  ['store', '存储事件'],
  ['errors', '仅错误'],
  ['noreplay', '隐藏回放'],
]

function renderEvents() {
  $('#ev-count').textContent = state.events.length || ''
  setHTML('#ev-filter', EVENT_FILTERS.map(([k, label]) => `
    <button data-evfilter="${k}" class="rounded-full px-3 py-1 text-xs ring-1 transition ${state.eventFilter === k ? 'bg-indigo-500/20 text-indigo-200 ring-indigo-500/40' : 'text-slate-400 ring-white/5 hover:text-slate-200'}">${label}</button>`).join(''))
  let events = state.events
  const f = state.eventFilter
  if (f === 'engine') events = events.filter(e => e.source === 'engine')
  if (f === 'store') events = events.filter(e => e.source !== 'engine')
  if (f === 'errors') events = events.filter(e => e.event.error)
  if (f === 'noreplay') events = events.filter(e => !e.event.replayed && e.event.type !== 'action_replayed')
  if (!events.length) {
    setHTML('#p-events', '<div class="text-sm text-slate-500 py-10 text-center">暂无事件</div>')
    return
  }
  const shown = events.slice(-800).reverse()
  let lastExec = null
  setHTML('#p-events', shown.map(e => {
    const ev = e.event
    const [dot, label] = EVENT_STYLE[ev.type] || ['bg-slate-500', ev.type]
    const divider = ev.execution_id && ev.execution_id !== lastExec && ev.type === 'drive_finished'
    lastExec = ev.execution_id || lastExec
    const dur = ev.duration_ns ? fmtDur(ev.duration_ns / 1e6) : ''
    const details = []
    if (ev.kind) details.push(kindBadge(ev.kind))
    if (ev.status) details.push(chip(ev.status))
    if (ev.replayed) details.push('<span class="chip bg-slate-500/10 text-slate-400 ring-1 ring-white/5">replayed</span>')
    if (ev.record_version) details.push(`<span class="text-[11px] text-slate-500">v${ev.record_version}</span>`)
    if (ev.generation) details.push(`<span class="text-[11px] text-slate-500">gen ${ev.generation}</span>`)
    if (ev.pending?.length) details.push(`<span class="text-[11px] text-amber-300/80">等待 ${ev.pending.map(p => esc(p.Key || p.key)).join(', ')}</span>`)
    if (ev.winner) details.push(`<span class="text-[11px] text-emerald-300">winner ${esc(ev.winner)}</span>`)
    return `${divider ? '<li class="pt-3 pb-1"><div class="h-px bg-white/5"></div></li>' : ''}
    <li class="group flex gap-3 rounded-lg px-3 py-2 hover:bg-white/[.02] ${ev.error ? 'bg-rose-500/[.04]' : ''}">
      <div class="pt-1.5"><span class="block h-2 w-2 rounded-full ${dot}"></span></div>
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-2 flex-wrap">
          <span class="text-sm text-slate-200 font-medium">${esc(label)}</span>
          <button data-record="${esc(ev.key)}" class="font-mono text-xs text-indigo-300/90 hover:text-indigo-200 truncate max-w-[340px]">${esc(ev.key)}</button>
          ${details.join('')}
        </div>
        ${ev.error ? `<div class="mt-1 text-xs font-mono text-rose-300/90 break-all">[${esc(ev.error.category)}${ev.error.operation ? ' · ' + esc(ev.error.operation) : ''}] ${esc(ev.error.message)}</div>` : ''}
      </div>
      <div class="text-right shrink-0">
        <div class="text-[11px] font-mono text-slate-500">${fmtTime(ev.at || e.at)}</div>
        <div class="text-[11px] text-slate-600">${dur}${e.source === 'engine' ? '' : ' · store'}</div>
      </div>
    </li>`
  }).join(''))
}

// ---------- messages ----------

function renderMessages() {
  const msgs = state.detail.messages
  if (!msgs.length) {
    setHTML('#p-messages', '<div class="text-sm text-slate-500 py-10 text-center">暂无通知</div>')
    return
  }
  const stateCls = {
    PENDING: 'text-amber-300 bg-amber-500/10 ring-amber-500/20',
    ACKED: 'text-emerald-300 bg-emerald-500/10 ring-emerald-500/20',
    DONE: 'text-emerald-300 bg-emerald-500/10 ring-emerald-500/20',
    REMOVED: 'text-slate-400 bg-slate-500/10 ring-white/5',
  }
  const purposeCls = { REQUEST: 'text-fuchsia-300', WAKE: 'text-cyan-300', CANCEL: 'text-slate-400' }
  setHTML('#p-messages', `
    <p class="text-xs text-slate-500 mb-3">REQUEST 表示需要外部处理的请求，WAKE 会让 worker 重新 Drive，CANCEL 通知外部停止失败的等待。</p>
    <div class="rounded-xl ring-1 ring-white/5 overflow-hidden">
      <div class="grid grid-cols-[90px_minmax(0,1fr)_90px_150px_150px] gap-4 px-4 py-2.5 text-[11px] uppercase tracking-wider text-slate-500 bg-slate-900/70 border-b border-white/5">
        <div>Purpose</div><div>Key</div><div>状态</div><div>可见时间</div><div>处理时间</div>
      </div>
      ${msgs.map(m => `
        <div class="grid grid-cols-[90px_minmax(0,1fr)_90px_150px_150px] gap-4 px-4 py-2.5 items-center border-b border-white/[.03] last:border-0 text-sm">
          <div class="font-mono text-xs font-semibold ${purposeCls[m.purpose] || ''}">${esc(m.purpose)}</div>
          <div class="font-mono text-xs text-slate-300 truncate" title="${esc(m.id)}">${esc(m.key)}</div>
          <div><span class="inline-flex rounded-md px-1.5 py-0.5 text-[11px] ring-1 ${stateCls[m.state] || ''}">${esc(m.state)}</span></div>
          <div class="text-xs text-slate-400 font-mono">${fmtTime(m.available_at)}</div>
          <div class="text-xs text-slate-500 font-mono">${fmtTime(m.done_at)}</div>
        </div>`).join('')}
    </div>`)
}

function renderIO() {
  const { run } = state.detail
  setHTML('#p-io', `
    <div class="grid lg:grid-cols-2 gap-5">
      <div><div class="text-xs uppercase tracking-wider text-slate-500 mb-2">Input</div>${jsonBlock(decodeRaw(run.input))}</div>
      <div><div class="text-xs uppercase tracking-wider text-slate-500 mb-2">Output</div>${jsonBlock(decodeRaw(run.output))}</div>
    </div>
    ${run.pending?.length ? `<div class="mt-6"><div class="text-xs uppercase tracking-wider text-slate-500 mb-2">最近一次 Drive 报告的 Pending</div>${jsonBlock(run.pending)}</div>` : ''}`)
}

// ---------- waits (right panel) ----------

function renderWaits() {
  const { records, groups } = state.detail
  const waits = records.filter(r => r.status === 'SUSPENDED' && (r.kind === 'SIGNAL' || r.kind === 'SLEEP'))
  $('#wait-count').textContent = waits.length ? `${waits.length} 个` : ''
  setHTML('#wait-ping', waits.length
    ? '<span class="absolute inline-flex h-full w-full rounded-full bg-amber-400 opacity-60 animate-ping"></span><span class="relative inline-flex h-2 w-2 rounded-full bg-amber-400"></span>'
    : '<span class="relative inline-flex h-2 w-2 rounded-full bg-slate-600"></span>')
  // Re-rendering would steal focus from an editor; skip when nothing changed.
  const signature = state.current + '|' + waits.map(r => `${r.key}@${r.version}@${r.until}`).join(',') + '|' + groups.map(g => g.state.Status).join(',')
  if (signature === state.waitSignature) return
  state.waitSignature = signature
  // Preserve drafts the user is typing across live refreshes.
  for (const el of $$('[data-draft]')) state.drafts[el.dataset.draft] = el.value
  if (!waits.length) {
    $('#p-waits').innerHTML = `<div class="text-center py-14 px-4">
      <div class="mx-auto h-10 w-10 rounded-xl bg-slate-800 grid place-items-center mb-3"><svg class="h-5 w-5 text-slate-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M5 13l4 4L19 7"/></svg></div>
      <div class="text-sm text-slate-400">没有等待中的信号</div>
      <div class="text-xs text-slate-600 mt-1">WaitFor / Sleep / WaitAny 挂起时会出现在这里</div></div>`
    return
  }
  const groupOf = Object.fromEntries(groups.map(g => [g.key, g]))
  $('#p-waits').innerHTML = waits.map(r => r.kind === 'SIGNAL' ? signalCard(r, groupOf[r.group]) : sleepCard(r, groupOf[r.group])).join('')
}

function groupNote(group) {
  if (!group) return ''
  const others = group.requests.length - 1
  return `<div class="mt-2 text-[11px] text-fuchsia-300/80 flex items-center gap-1.5">
    <svg class="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M8 7h12M8 12h12M8 17h12M4 7h.01M4 12h.01M4 17h.01"/></svg>
    WaitAny <span class="font-mono">${esc(group.key)}</span> · 胜出将取消其余 ${others} 个成员</div>`
}

function signalCard(r, group) {
  const draft = state.drafts[r.key] ?? '{\n  \n}'
  return `<div class="rounded-xl bg-slate-900 ring-1 ring-fuchsia-500/20 shadow-lg shadow-fuchsia-500/[.03] overflow-hidden fade-up">
    <div class="px-4 py-3 border-b border-white/5">
      <div class="flex items-center justify-between gap-2">${kindBadge('SIGNAL')}<span class="text-[11px] text-slate-500">${ago(r.started_at)}</span></div>
      <div class="mt-2 font-mono text-sm text-white break-all">${esc(r.key)}</div>
      ${groupNote(group)}
    </div>
    <div class="px-4 py-3 space-y-3">
      <div>
        <div class="text-[11px] uppercase tracking-wider text-slate-500 mb-1.5">Request</div>
        ${jsonBlock(decodeRaw(r.input), 'max-h-40')}
      </div>
      <div>
        <div class="flex items-center justify-between mb-1.5">
          <span class="text-[11px] uppercase tracking-wider text-slate-500">Response JSON</span>
          <span class="flex gap-1">
            <button class="btn-ghost text-[11px]" data-preset='"approved"' data-key="${esc(r.key)}">"approved"</button>
            <button class="btn-ghost text-[11px]" data-preset="true" data-key="${esc(r.key)}">true</button>
            <button class="btn-ghost text-[11px]" data-format="${esc(r.key)}">格式化</button>
          </span>
        </div>
        <textarea data-draft="${esc(r.key)}" spellcheck="false" rows="6"
          class="editor w-full rounded-lg border-0 bg-slate-950 ring-1 ring-white/10 focus:ring-2 focus:ring-fuchsia-500 font-mono text-xs text-slate-200 p-3">${esc(draft)}</textarea>
        <div data-jsonerr="${esc(r.key)}" class="hidden mt-1 text-[11px] text-rose-300"></div>
      </div>
      <div class="flex items-center gap-2">
        <button class="btn-primary flex-1" data-submit="${esc(r.key)}">
          <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="m5 12 14-7-5 14-3-6z"/></svg>提交并推进
        </button>
        ${group ? `<button class="btn-danger" data-fail="${esc(r.key)}" title="将该成员标记为失败">标记失败</button>` : ''}
      </div>
    </div>
  </div>`
}

function sleepCard(r, group) {
  return `<div class="rounded-xl bg-slate-900 ring-1 ring-cyan-500/20 overflow-hidden fade-up">
    <div class="px-4 py-3">
      <div class="flex items-center justify-between gap-2">${kindBadge('SLEEP')}<span class="text-[11px] text-cyan-300 tabular-nums" data-countdown="${esc(r.until)}">${countdown(r.until)}</span></div>
      <div class="mt-2 font-mono text-sm text-white break-all">${esc(r.key)}</div>
      <div class="mt-1 text-xs text-slate-500">到期时间 ${fmtDate(r.until)}</div>
      ${groupNote(group)}
      <div class="mt-3 flex gap-2">
        <button class="btn-secondary flex-1" data-fire="${esc(r.key)}">
          <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M13 2 4 14h7l-1 8 9-12h-7z"/></svg>立即唤醒
        </button>
        ${group ? `<button class="btn-danger" data-fail="${esc(r.key)}">标记失败</button>` : ''}
      </div>
    </div>
  </div>`
}

// ---------- drawer ----------

function openDrawer(key, refresh = false) {
  const record = state.detail?.records.find(r => r.key === key)
  if (!record) {
    if (!refresh) toast(`没有记录 ${key}`, 'warn')
    return
  }
  state.drawerKey = key
  $('#drawer').classList.remove('hidden')
  setHTML('#dr-kind', `${kindBadge(record.kind)} <span class="ml-1">${chip(record.status)}</span>`)
  $('#dr-key').textContent = record.key
  const related = state.events.filter(e => e.event.key === key).slice(-50).reverse()
  const field = (k, v) => `<div><div class="text-[11px] uppercase tracking-wider text-slate-500">${k}</div><div class="mt-0.5 text-sm text-slate-200 font-mono">${v}</div></div>`
  setHTML('#dr-body', `
    <div class="grid grid-cols-3 gap-4">
      ${field('Version', 'v' + record.version)}
      ${field('Started', fmtTime(record.started_at))}
      ${field('Ended', fmtTime(record.ended_at))}
      ${record.kind === 'SLEEP' ? field('Until', fmtDate(record.until)) : ''}
      ${record.group ? field('Group', esc(record.group)) : ''}
      ${field('Updated', ago(record.updated_at))}
    </div>
    ${record.error ? `<div class="rounded-lg bg-rose-500/10 ring-1 ring-rose-500/20 px-4 py-3 text-sm text-rose-200 font-mono break-all">${esc(record.error)}</div>` : ''}
    <div><div class="text-[11px] uppercase tracking-wider text-slate-500 mb-1.5">Input</div>${jsonBlock(decodeRaw(record.input))}</div>
    <div><div class="text-[11px] uppercase tracking-wider text-slate-500 mb-1.5">Result</div>${jsonBlock(decodeRaw(record.result))}</div>
    <div>
      <div class="text-[11px] uppercase tracking-wider text-slate-500 mb-1.5">相关事件 (${related.length})</div>
      <div class="space-y-1">${related.map(e => {
        const [dot, label] = EVENT_STYLE[e.event.type] || ['bg-slate-500', e.event.type]
        return `<div class="flex items-center gap-2 text-xs rounded-md px-2 py-1.5 bg-slate-950/50">
          <span class="h-1.5 w-1.5 rounded-full ${dot}"></span><span class="text-slate-300">${esc(label)}</span>
          ${e.event.replayed ? '<span class="text-slate-500">replayed</span>' : ''}
          ${e.event.error ? `<span class="text-rose-300 truncate">${esc(e.event.error.message)}</span>` : ''}
          <span class="ml-auto font-mono text-slate-500">${fmtTime(e.event.at || e.at)}</span></div>`
      }).join('') || '<div class="text-xs text-slate-600">无</div>'}</div>
    </div>`)
}

function closeDrawer() {
  state.drawerKey = null
  $('#drawer').classList.add('hidden')
}

// ---------- actions ----------

async function doSubmit(key) {
  const el = $(`[data-draft="${CSS.escape(key)}"]`)
  const errEl = $(`[data-jsonerr="${CSS.escape(key)}"]`)
  let value
  try {
    value = JSON.parse(el.value)
    errEl.classList.add('hidden')
  } catch (err) {
    errEl.textContent = 'JSON 无效：' + err.message
    errEl.classList.remove('hidden')
    return
  }
  try {
    const res = await api(runPath(state.current, '/submit'), { method: 'POST', body: { key, response: value } })
    delete state.drafts[key]
    toast(res.OK ? `已提交 ${key}，worker 将重新 Drive` : `${key} 已被处理，本次提交被忽略`, res.OK ? 'ok' : 'warn')
  } catch (err) { toast(err.message, 'err') }
}

async function doFire(key) {
  try {
    await api(runPath(state.current, '/fire'), { method: 'POST', body: { key } })
    toast(`已唤醒 ${key}`)
  } catch (err) { toast(err.message, 'err') }
}

async function doFail(key) {
  const message = prompt(`将 ${key} 标记为失败，原因：`, 'failed from debugger')
  if (message === null) return
  try {
    const res = await api(runPath(state.current, '/fail'), { method: 'POST', body: { key, message } })
    toast(res.OK ? `${key} 已标记失败` : '成员已结束，未改变', res.OK ? 'ok' : 'warn')
  } catch (err) { toast(err.message, 'err') }
}

// ---------- wiring ----------

document.addEventListener('click', e => {
  const t = e.target.closest('button, [data-close]')
  if (!t) return
  if (t.dataset.close !== undefined) return closeDrawer()
  if (t.dataset.status) { state.statusFilter = t.dataset.status; return renderRuns() }
  if (t.dataset.evfilter) { state.eventFilter = t.dataset.evfilter; return renderEvents() }
  if (t.dataset.tab) {
    state.tab = t.dataset.tab
    $$('#tabs .tab').forEach(b => b.classList.toggle('tab-active', b === t))
    $$('[data-panel]').forEach(p => p.classList.toggle('hidden', p.dataset.panel !== state.tab))
    return
  }
  if (t.dataset.record) return openDrawer(t.dataset.record)
  if (t.dataset.submit) return doSubmit(t.dataset.submit)
  if (t.dataset.fire) return doFire(t.dataset.fire)
  if (t.dataset.fail) return doFail(t.dataset.fail)
  if (t.dataset.preset) {
    const el = $(`[data-draft="${CSS.escape(t.dataset.key)}"]`)
    el.value = JSON.stringify(JSON.parse(t.dataset.preset), null, 2)
    state.drafts[t.dataset.key] = el.value
    return
  }
  if (t.dataset.format) {
    const el = $(`[data-draft="${CSS.escape(t.dataset.format)}"]`)
    try { el.value = JSON.stringify(JSON.parse(el.value), null, 2); state.drafts[t.dataset.format] = el.value } catch (err) { toast('JSON 无效：' + err.message, 'err') }
  }
})

document.addEventListener('input', e => {
  if (e.target.dataset.draft) state.drafts[e.target.dataset.draft] = e.target.value
})

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') closeDrawer()
  if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && e.target.dataset.draft) doSubmit(e.target.dataset.draft)
})

$('#search').addEventListener('input', e => { state.search = e.target.value; renderRuns() })

$('#btn-redrive').addEventListener('click', async () => {
  try {
    await api(runPath(state.current, '/redrive'), { method: 'POST', body: {} })
    toast('已入队 WAKE，在线 worker 会重新 Drive')
  } catch (err) { toast(err.message, 'err') }
})

$('#btn-delete').addEventListener('click', async () => {
  if (!confirm(`删除 Run ${state.current} 的全部记录、事件与通知？`)) return
  try {
    await api(runPath(state.current), { method: 'DELETE' })
    location.hash = ''
    toast('已删除')
  } catch (err) { toast(err.message, 'err') }
})

function route() {
  const m = location.hash.match(/^#\/runs\/(.+)$/)
  selectRun(m ? decodeURIComponent(m[1]) : null)
}
window.addEventListener('hashchange', route)

// Live updates: SSE tells us which run changed; coalesce refetches.
let pendingRuns = false, pendingDetail = false, timer = null
function schedule(run) {
  pendingRuns = true
  if (run === state.current) pendingDetail = true
  if (timer) return
  timer = setTimeout(async () => {
    timer = null
    const [r, d] = [pendingRuns, pendingDetail]
    pendingRuns = pendingDetail = false
    await Promise.all([r && loadRuns().catch(() => {}), d && refreshDetail()])
  }, 120)
}

function connect() {
  const es = new EventSource('/api/stream')
  const set = (ok, text) => {
    $('#conn-dot').className = `h-1.5 w-1.5 rounded-full ${ok ? 'bg-emerald-400' : 'bg-rose-400'}`
    $('#conn-text').textContent = text
  }
  es.onopen = () => { set(true, '实时连接'); loadRuns().catch(() => {}); if (state.current) refreshDetail() }
  es.onerror = () => set(false, '连接断开，重连中…')
  es.addEventListener('run', e => schedule(JSON.parse(e.data)))
}

// Clocks: countdowns, "ago" labels and worker liveness.
setInterval(() => {
  for (const el of $$('[data-countdown]')) el.textContent = countdown(el.dataset.countdown)
}, 1000)
setInterval(() => { loadRuns().catch(() => {}); if (state.current && state.detail) refreshDetail() }, 5000)

loadRuns().catch(err => toast(err.message, 'err')).finally(route)
connect()
