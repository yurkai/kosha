import { spawn, execFileSync } from 'node:child_process'
import { once } from 'node:events'
import { resolve } from 'node:path'
import { readFileSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'
import WebSocket from 'ws'

export { sleep }

export async function waitFor(predicate, label, timeout = 15_000) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    if (await predicate()) return
    await sleep(25)
  }
  throw new Error(`Timed out: ${label}`)
}

export async function startServer(database, args = [], { envFile = '', address: listenAddress = '127.0.0.1:0', env = {} } = {}) {
  // Personal settings must not change the limits or ports of automated checks.
  const processEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('KOSHA_')))
  const child = spawn(resolve(process.env.KOSHA_BINARY || 'bin/kosha'), [
    '-env-file', envFile, ...(listenAddress === null ? [] : ['-addr', listenAddress]),
    '-db', database, '-metrics', ...args,
  ], { stdio: ['ignore', 'pipe', 'pipe'], env: { ...processEnv, ...env } })
  let logs = ''
  let address
  let spawnError
  child.on('error', e => { spawnError = e })
  for (const stream of [child.stdout, child.stderr]) stream.on('data', data => {
    logs = (logs + data.toString()).slice(-64_000)
    for (const line of logs.split('\n')) {
      try { const event = JSON.parse(line); if (event.msg === 'listening') address = event.address } catch {}
    }
  })
  try {
    await waitFor(() => {
      if (spawnError) throw spawnError
      if (child.exitCode !== null) throw new Error(`Server exited: ${logs}`)
      return address
    }, 'server startup')
    const url = `http://${address}`
    const response = await fetch(`${url}/healthz`)
    if (!response.ok) throw new Error('Server health check failed')
    return {
      child, url, logs: () => logs,
      async createRoom() {
        const response = await fetch(`${url}/api/rooms`, { method: 'POST' })
        if (response.status !== 201) throw new Error(`Room creation HTTP ${response.status}`)
        const room = await response.json()
        if (!/^[A-Z0-9]{6}$/.test(room.id) || room.path !== `/${room.id}`) throw new Error('Invalid short room response')
        return room.id
      },
      async metrics() {
        const response = await fetch(`${url}/debug/metrics`)
        if (!response.ok) throw new Error(`Metrics HTTP ${response.status}`)
        return response.json()
      },
      async stop(signal = 'SIGTERM') {
        if (child.exitCode !== null || child.signalCode !== null) return
        const exited = once(child, 'exit')
        child.kill(signal)
        let timer
        try {
          const [code, exitSignal] = await Promise.race([
            exited,
            new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('Server shutdown timeout')), 15_000) }),
          ])
          if (signal === 'SIGTERM' && (code !== 0 || exitSignal)) throw new Error(`Unclean shutdown: ${code}/${exitSignal}\n${logs}`)
        } catch (error) { child.kill('SIGKILL'); throw error }
        finally { clearTimeout(timer) }
      },
    }
  } catch (error) { child.kill('SIGKILL'); throw error }
}

export function connectClient(server, room) {
  const doc = new Y.Doc()
  const provider = new WebsocketProvider(server.url.replace(/^http/, 'ws') + '/yjs', room, doc, {
    WebSocketPolyfill: WebSocket, disableBc: true, maxBackoffTime: 250,
  })
  return {
    doc, provider, text: doc.getText('code'),
    ready: () => waitFor(() => provider.synced, `client sync in ${room}`),
    destroy() { provider.destroy(); doc.destroy() },
  }
}

export function converged(clients) {
  const expected = clients[0].text.toString()
  return clients.every(c => c.provider.synced && c.text.toString() === expected)
}

export function rssBytes(pid) {
  if (process.platform === 'linux') {
    const status = readFileSync(`/proc/${pid}/status`, 'utf8')
    const match = status.match(/^VmRSS:\s+(\d+)\s+kB$/m)
    if (!match) throw new Error('Linux RSS unavailable')
    return Number(match[1]) * 1024
  }
  if (process.platform === 'darwin') {
    return Number(execFileSync('/bin/ps', ['-o', 'rss=', '-p', String(pid)], { encoding: 'utf8' }).trim()) * 1024
  }
  throw new Error(`RSS sampling is not implemented for ${process.platform}`)
}
