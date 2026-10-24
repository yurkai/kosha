import assert from 'node:assert/strict'
import { mkdir, mkdtemp, writeFile } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { startServer, connectClient, converged, waitFor, sleep } from './lib/harness.mjs'

await mkdir('artifacts', { recursive: true })
const directory = await mkdtemp(resolve('artifacts/retention-'))
const database = join(directory, 'test.db')
const envFile = join(directory, '.env')
await writeFile(envFile, 'KOSHA_ROOM_TTL=2s\n', { mode: 0o600 })
const report = { started_at: new Date().toISOString(), ttl: '2s', checks: {}, status: 'RUNNING' }
let server
const clients = new Set()
const connect = room => { const c = connectClient(server, room); clients.add(c); return c }
const destroy = c => { c.destroy(); clients.delete(c) }
const status = room => fetch(`${server.url}/api/rooms/${room}`).then(r => r.status)
const released = () => waitFor(async () => {
  const m = await server.metrics()
  return m.resident_rooms === 0 && m.websocket_handlers === 0
}, 'last participant and persistence drain')
const boot = async () => { server = await startServer(database, [], { envFile }) }

try {
  await boot()
  const room = await server.createRoom(), unused = await server.createRoom()
  const a = connect(room), offline = connect(room)
  await Promise.all([a.ready(), offline.ready()])
  a.text.insert(0, 'SELECT 42; — сохранённый текст 🐾\n')
  a.doc.getMap('settings').set('language', 'sql')
  await waitFor(() => converged([a, offline]), 'initial edits converge')
  const expected = a.text.toString()

  await waitFor(() => server.logs().includes('expired inactive rooms'), 'periodic cleanup without visiting old rooms', 6000)
  assert.equal(await status(unused), 410)
  assert.equal(await status(room), 200)
  report.checks.periodic_cleanup_and_active_room_protection = 'PASS'

  destroy(a)
  await waitFor(async () => (await server.metrics()).websocket_handlers === 1, 'one remaining participant')
  await sleep(2200)
  assert.equal(await status(room), 200)
  assert.equal(offline.text.toString(), expected)
  report.checks.one_remaining_participant_protects_room = 'PASS'

  offline.provider.disconnect()
  offline.text.insert(0, 'OFFLINE MUST NOT RESURRECT\n')
  await released()
  await sleep(600)
  assert.equal(await status(room), 200)
  await server.stop()
  await boot()
  const fresh = connect(room)
  await fresh.ready()
  assert.equal(fresh.text.toString(), expected)
  assert.equal(fresh.doc.getMap('settings').get('language'), 'sql')
  destroy(fresh)
  await released()
  report.checks.last_leave_resets_ttl_and_restart_preserves_document = 'PASS'

  await server.stop()
  await sleep(2200)
  await boot()
  assert.equal(await status(room), 410)
  assert.equal((await fetch(`${server.url}/${room}`)).status, 410)
  assert.equal((await fetch(`${server.url}/api/rooms/${room}/visit`, { method: 'POST' })).status, 410)
  report.checks.startup_cleanup_and_permanent_expired_link = 'PASS'

  let rejected = false
  offline.provider.on('connection-error', () => { rejected = true })
  offline.provider.serverUrl = server.url.replace(/^http/, 'ws') + '/yjs'
  offline.provider.connect()
  await waitFor(() => rejected, 'old offline client rejected')
  await sleep(500)
  assert.equal(offline.provider.synced, false)
  assert.equal(await status(room), 410)
  destroy(offline)
  report.checks.offline_client_cannot_restore_deleted_document = 'PASS'

  const replacement = await server.createRoom()
  assert.notEqual(replacement, room)
  const clean = connect(replacement)
  await clean.ready()
  assert.equal(clean.text.toString(), '')
  destroy(clean)
  await released()
  report.checks.new_room_is_independent_and_empty = 'PASS'
  report.status = 'PASS'
} catch (error) {
  report.status = 'FAIL'
  report.error = String(error)
  throw error
} finally {
  for (const client of clients) client.destroy()
  try { if (server) await server.stop() }
  finally {
    report.finished_at = new Date().toISOString()
    await writeFile(join(directory, 'report.json'), JSON.stringify(report, null, 2) + '\n')
    await writeFile('artifacts/latest-retention-report.json', JSON.stringify(report, null, 2) + '\n')
    console.log(JSON.stringify(report, null, 2))
  }
}
