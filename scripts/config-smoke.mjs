import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtemp, rm, writeFile, access } from 'node:fs/promises'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { startServer } from './lib/harness.mjs'

async function unusedPort() {
  const listener = createServer()
  await new Promise((resolve, reject) => {
    listener.once('error', reject)
    listener.listen(0, '127.0.0.1', resolve)
  })
  const { port } = listener.address()
  await new Promise((resolve, reject) => listener.close(error => error ? reject(error) : resolve()))
  return port
}

const directory = await mkdtemp(join(tmpdir(), 'kosha-config-'))
try {
  const envFile = join(directory, '.env')
  for (const source of ['file', 'process', 'flags']) {
    const port = await unusedPort()
    await writeFile(envFile, `KOSHA_HOST=127.0.0.1\nKOSHA_PORT=${source === 'file' ? port : 1}\n`, { mode: 0o600 })
    const env = source === 'process' ? { KOSHA_PORT: String(port) } : source === 'flags' ? { KOSHA_PORT: 'invalid' } : {}
    const args = source === 'flags' ? ['-addr', `127.0.0.1:${port}`] : []
    const server = await startServer(join(directory, `${source}.db`), args, { envFile, address: null, env })
    try {
      assert.equal(server.url, `http://127.0.0.1:${port}`)
      const room = await server.createRoom()
      assert.equal((await fetch(`${server.url}/${room}`)).status, 200)
      console.log(`PASS: running server uses port from ${source}`)
    } finally { await server.stop() }
  }

  const database = join(directory, 'invalid.db')
  await writeFile(envFile, 'KOSHA_PORT=70000\n')
  const result = spawnSync(resolve(process.env.KOSHA_BINARY || 'bin/kosha'), ['-env-file', envFile, '-db', database], {
    encoding: 'utf8', timeout: 10_000,
    env: Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('KOSHA_'))),
  })
  assert.ifError(result.error)
  assert.equal(result.status, 2, result.stderr)
  assert.match(result.stderr, /invalid configuration/)
  await assert.rejects(access(database), { code: 'ENOENT' })
  console.log('PASS: invalid configuration exits before creating the database')
} finally { await rm(directory, { recursive: true, force: true }) }
