import assert from 'node:assert/strict'
import { mkdir, mkdtemp, writeFile, stat } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { startServer, connectClient, converged, waitFor, rssBytes, sleep } from './lib/harness.mjs'

const duration = Number(process.argv.find(a => a.startsWith('--duration='))?.split('=')[1] || 60)
assert(Number.isFinite(duration) && duration >= 5 && duration <= 3600, 'duration must be 5..3600 seconds')
// Each y-websocket provider intentionally installs one exit listener. Allow
// the 20-client workload, and verify that destruction removes every listener.
const exitListenersBefore = process.listenerCount('exit')
process.setMaxListeners(Math.max(process.getMaxListeners(), exitListenersBefore + 25))
await mkdir('artifacts', { recursive: true })
const workdir = await mkdtemp(resolve('artifacts/verify-'))
const database = join(workdir, 'test.db')
const report = {
  started_at: new Date().toISOString(), driver: { platform: process.platform, arch: process.arch, node: process.version },
  workload: { rooms: 5, peers_per_room: 4, seed_bytes_per_room: 102400, duration_seconds: duration, tick_ms: 200 },
  checks: {}, samples: [], limitations: [
    'RSS includes only the separately compiled Go server, excluding the driver and browsers.',
    'A short local test is not a Linux VPS/VPN capacity guarantee or a long-running leak test.',
    'WebSocket sync is not a durable-save acknowledgement; the restart test below uses graceful shutdown.',
  ],
}
let server
let clients = []
const client = (room) => { const c = connectClient(server, room); clients.push(c); return c }
const destroyClients = () => {
  for (const c of clients) c.destroy()
  clients = []
  assert.equal(process.listenerCount('exit'), exitListenersBefore, 'Yjs clients must remove their exit listeners')
}
const sample = async (phase) => {
  const metrics = await server.metrics()
  const entry = { phase, pid: server.child.pid, elapsed_ms: Date.now() - started, rss_bytes: rssBytes(server.child.pid), ...metrics }
  assert(entry.rss_bytes > 0, 'RSS must be measured, not silently omitted')
  report.samples.push(entry)
  return entry
}
const started = Date.now()

try {
  server = await startServer(database)
  report.server_pid = server.child.pid
  await sample('idle')
  const a = client('functional'), b = client('functional'), other = client('isolated')
  await Promise.all(clients.map(c => c.ready()))
  a.text.insert(0, 'alpha: Привет 👋\n')
  b.text.insert(0, 'beta: SELECT 1;\n')
  await waitFor(() => converged([a, b]), 'concurrent inserts converge')
  assert(a.text.toString().includes('alpha: Привет 👋'))
  assert(a.text.toString().includes('beta: SELECT 1;'))
  assert.equal(other.text.toString(), '', 'rooms must be isolated')
  a.text.insert(0, 'REMOVE:')
  await waitFor(() => converged([a, b]), 'insert before delete')
  b.text.delete(0, 7)
  await waitFor(() => converged([a, b]), 'remote deletion')
  assert(!a.text.toString().includes('REMOVE:'))
  report.checks.concurrent_editing_and_isolation = 'PASS'

  a.provider.disconnect()
  a.text.insert(0, 'OFFLINE 🐾\n')
  b.text.insert(0, 'ONLINE\n')
  a.provider.connect()
  await waitFor(() => converged([a, b]), 'offline edits merge after reconnect')
  assert.equal(a.text.toString().split('OFFLINE 🐾').length - 1, 1)
  assert.equal(a.text.toString().split('ONLINE').length - 1, 1)
  report.checks.offline_edits_and_reconnection = 'PASS'

  const expected = a.text.toString()
  // Keep clients connected: SIGTERM must drain pending writes, not rely on room eviction.
  await server.stop()
  destroyClients()
  server = await startServer(database)
  const restored = client('functional'), fresh = client('functional')
  await Promise.all(clients.map(c => c.ready()))
  assert.equal(restored.text.toString(), expected, 'new process must restore SQLite state')
  assert.equal(fresh.text.toString(), expected, 'new Y.Doc must not duplicate restored content')
  report.checks.sqlite_recovery_after_process_restart = 'PASS'
  destroyClients()
  await waitFor(async () => (await server.metrics()).resident_rooms === 0, 'functional rooms unload')
  report.server_pid = server.child.pid
  await sample('before_load')

  const groups = Array.from({ length: 5 }, (_, r) => Array.from({ length: 4 }, () => client(`load-${r}`)))
  await Promise.all(clients.map(c => c.ready()))
  for (const [r, peers] of groups.entries()) {
    const marker = `# room ${r}\n`
    peers[0].text.insert(0, marker + '# x = 42; SELECT 1;\n'.repeat(6000).slice(0, 102400 - marker.length))
  }
  await waitFor(() => groups.every(converged), 'seed documents converge')
  const connected = await sample('connected_seeded')
  assert.equal(connected.resident_rooms, 5)
  assert.equal(connected.websocket_handlers, 20)
  let ticks = 0, edits = 0, didReconnect = false
  const loadStart = Date.now()
  let nextSample = 0
  while (Date.now() - loadStart < duration * 1000) {
    for (const [r, peers] of groups.entries()) for (const [p, c] of peers.entries()) {
      c.doc.transact(() => {
        const offset = 32 + ((ticks * 7919 + p * 997) % Math.max(1, c.text.length - 64))
        if ((ticks + p) % 3 === 0) { c.text.delete(offset, 8); edits++ }
        c.text.insert(offset, `r${r}p${p}:${ticks};`)
        edits++
      })
    }
    ticks++
    if (!didReconnect && Date.now() - loadStart > duration * 500) {
      for (const [r, peers] of groups.entries()) {
        const c = peers[3]
        const onStatus = ({ status }) => {
          if (status === 'disconnected') {
            c.text.insert(0, `AUTO-RECONNECT-${r}\n`)
            c.provider.off('status', onStatus)
          }
        }
        c.provider.on('status', onStatus)
        c.provider.ws.terminate()
      }
      didReconnect = true
    }
    if (Date.now() >= nextSample) {
      await sample('load')
      nextSample = Date.now() + 1000
    }
    await sleep(200)
  }
  await waitFor(() => groups.every(converged), 'all 20 clients converge after sustained edits', 30_000)
  for (const [r, peers] of groups.entries()) {
    assert.equal(peers[0].text.toString().split(`AUTO-RECONNECT-${r}`).length - 1, 1)
    for (const [otherRoom] of groups.entries()) if (otherRoom !== r) {
      assert(!peers[0].text.toString().includes(`AUTO-RECONNECT-${otherRoom}`))
    }
  }
  report.workload.ticks = ticks
  report.workload.edit_operations = edits
  report.workload.actual_load_seconds = (Date.now() - loadStart) / 1000
  report.workload.final_utf8_bytes = groups.map(peers => Buffer.byteLength(peers[0].text.toString()))
  report.checks.automatic_reconnection_under_load = 'PASS'
  report.checks.five_rooms_four_peers_convergence = 'PASS'
  const expectedRooms = groups.map(peers => peers[0].text.toString())
  destroyClients()
  await waitFor(async () => {
    const m = await server.metrics()
    return m.resident_rooms === 0 && m.websocket_handlers === 0
  }, 'all load rooms unload', 30_000)
  await sample('released')
  await sleep(2000)
  await sample('released_after_2s')

  // Repeat full joins/leaves to catch obvious room retention and rehydration failures.
  for (let cycle = 0; cycle < 2; cycle++) {
    const again = groups.map((_, r) => Array.from({ length: 4 }, () => client(`load-${r}`)))
    await Promise.all(clients.map(c => c.ready()))
    for (const [r, peers] of again.entries()) for (const c of peers) assert.equal(c.text.toString(), expectedRooms[r])
    await sample(`churn_${cycle}_connected`)
    destroyClients()
    await waitFor(async () => {
      const m = await server.metrics()
      return m.resident_rooms === 0 && m.websocket_handlers === 0
    }, 'churn rooms unload')
    await sample(`churn_${cycle}_released`)
  }
  report.checks.repeated_room_release_and_reload = 'PASS'
  await server.stop()
  server = null
  report.database_bytes = (await stat(database)).size
  report.summary = {
    peak_rss_bytes: Math.max(...report.samples.map(s => s.rss_bytes)),
    peak_heap_alloc_bytes: Math.max(...report.samples.map(s => s.heap_alloc_bytes)),
    released: report.samples.at(-1),
  }
  report.status = 'PASS'
} catch (error) {
  report.status = 'FAIL'
  report.error = error.stack
  if (server) report.server_logs = server.logs()
  process.exitCode = 1
} finally {
  try { destroyClients() }
  catch (error) {
    report.status = 'FAIL'
    report.cleanup_error = String(error)
    process.exitCode = 1
  }
  if (server) {
    try { await server.stop() }
    catch (error) { report.status = 'FAIL'; report.shutdown_error = String(error); process.exitCode = 1 }
  }
  report.finished_at = new Date().toISOString()
  await writeFile(join(workdir, 'report.json'), JSON.stringify(report, null, 2) + '\n')
  await writeFile('artifacts/latest-report.json', JSON.stringify(report, null, 2) + '\n')
  console.log(JSON.stringify({ status: report.status, checks: report.checks, summary: report.summary, report: join(workdir, 'report.json'), error: report.error }, null, 2))
}
