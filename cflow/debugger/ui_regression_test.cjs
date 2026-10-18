// Run with: node --test cflow/debugger/ui_regression_test.cjs
const { readFileSync } = require('node:fs')
const { join } = require('node:path')
const vm = require('node:vm')
const test = require('node:test')
const assert = require('node:assert/strict')

function harness() {
  const elements = new Map()
  const element = selector => {
    if (!elements.has(selector)) elements.set(selector, {
      innerHTML: '',
      editors: [],
      classList: { toggle() {}, add() {}, remove() {} },
      querySelectorAll() { return this.editors },
      replaceChildren() { this.editors = []; this.innerHTML = '' },
    })
    return elements.get(selector)
  }
  const sandbox = {
    document: { querySelector: element, querySelectorAll: () => [] },
    location: { hash: '#/runs/B' },
    CSS: { escape: value => value },
  }
  vm.createContext(sandbox)
  const source = readFileSync(join(__dirname, 'ui/app.js'), 'utf8').split('// ---------- wiring ----------')[0]
  vm.runInContext(source + `
    renderRuns = () => {}
    renderDetail = () => {}
    toast = () => {}
    globalThis.app = {
      state, selectRun, refreshDetail, renderWaits, doSubmit,
      setAPI(fn) { api = fn },
    }
  `, sandbox)
  return { ...sandbox.app, element, location: sandbox.location }
}

const detail = id => ({
  run: { id },
  records: [{ key: 'main|approval', kind: 'SIGNAL', status: 'SUSPENDED', version: 1 }],
  groups: [],
})

test('switching runs isolates and restores same-key drafts', async () => {
  const app = harness()
  app.setAPI(async path => path.includes('/events') ? [] : detail(path.split('/')[3]))
  app.state.current = 'A'
  app.element('#p-waits').editors = [{ dataset: { draft: 'main|approval' }, value: '{"by":"A"}' }]
  app.state.drawerKey = 'main|approval'
  await app.selectRun('B')
  app.renderWaits()
  assert.equal(app.state.drafts['main|approval'], undefined)
  assert.equal(app.state.drawerKey, null)
  assert.ok(!app.element('#p-waits').innerHTML.includes('by'))
  app.element('#p-waits').editors = [{ dataset: { draft: 'main|approval' }, value: '{"by":"B"}' }]
  await app.selectRun('A')
  assert.equal(app.state.drafts['main|approval'], '{"by":"A"}')
  await app.selectRun('B')
  assert.equal(app.state.drafts['main|approval'], '{"by":"B"}')
})

test('late refresh cannot overwrite newer detail or rewind cursor', async () => {
  const app = harness()
  app.state.current = 'A'
  const requests = []
  app.setAPI(path => new Promise((resolve, reject) => requests.push({ path, resolve, reject })))
  const old = app.refreshDetail()
  const newer = app.refreshDetail()
  requests[2].resolve({ run: { id: 'A', status: 'COMPLETED' } })
  requests[3].resolve([{ id: 2 }, { id: 1 }, { id: 2 }])
  await newer
  requests[0].resolve({ run: { id: 'A', status: 'SUSPENDED' } })
  requests[1].resolve([{ id: 1 }])
  await old
  assert.equal(app.state.detail.run.status, 'COMPLETED')
  assert.deepEqual(Array.from(app.state.events, event => event.id), [1, 2])
  assert.equal(app.state.lastEventId, 2)
  const stale = app.refreshDetail()
  const latest = app.refreshDetail()
  requests[6].resolve({ run: { id: 'A', status: 'COMPLETED' } })
  requests[7].resolve([])
  await latest
  requests[4].reject(new Error('not found'))
  requests[5].resolve([])
  await stale
  assert.equal(app.location.hash, '#/runs/B')
})

test('submission remains bound to selected run while response is in flight', async () => {
  const app = harness()
  app.state.current = 'A'
  app.state.detail = detail('A')
  app.element('[data-draft="main|approval"]').value = '{"by":"A"}'
  let resolve
  let sent
  app.setAPI((path, options) => {
    sent = { path, options }
    return new Promise(done => { resolve = done })
  })
  const pending = app.doSubmit('main|approval')
  app.state.current = 'B'
  app.state.drafts = { 'main|approval': '{"by":"B"}' }
  resolve({ OK: true })
  await pending
  assert.equal(sent.path, '/api/runs/A/submit')
  assert.equal(app.state.drafts['main|approval'], '{"by":"B"}')
})
