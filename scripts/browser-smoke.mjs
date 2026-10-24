import assert from 'node:assert/strict'
import { mkdir, mkdtemp, readFile } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { chromium } from 'playwright'
import { startServer, waitFor, sleep } from './lib/harness.mjs'

await mkdir('artifacts', { recursive: true })
const dir = await mkdtemp(resolve('artifacts/browser-'))
const server = await startServer(join(dir, 'browser.db'))
let browser
try {
  browser = await chromium.launch({
    headless: true,
    ...(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : process.platform === 'darwin' ? { channel: 'chrome' } : {}),
  })
  const a = await browser.newContext(), b = await browser.newContext()
  const page = await a.newPage(), peer = await b.newPage()
  const errors = [], external = []
  for (const p of [page, peer]) {
    p.on('pageerror', error => errors.push(String(error)))
    p.on('request', request => { if (!request.url().startsWith(server.url)) external.push(request.url()) })
  }
  // Opening the landing page must allocate a short, directly reloadable URL.
  await page.goto(server.url)
  await waitFor(() => /^\/[A-Z0-9]{6}$/.test(new URL(page.url()).pathname), 'six-character room path')
  const shortUrl = page.url()
  await peer.goto(shortUrl)
  await waitFor(async () => (await peer.locator('#status').textContent()) === 'Подключено', 'join by short URL')
  await page.selectOption('#language', 'text')
  await waitFor(async () => (await peer.locator('#language').inputValue()) === 'text', 'shared Text mode')
  await page.locator('.cm-content').click()
  await page.keyboard.insertText('SELECT name FROM users;\ndef greet():\n    return "обычный текст"\n')
  const plain = page.locator('.cm-line span').filter({ hasText: /^(SELECT|def|return)$/ })
  assert.equal(await plain.count(), 0, 'Text must not highlight language tokens')
  const [download] = await Promise.all([page.waitForEvent('download'), page.click('#download')])
  assert.equal(download.suggestedFilename(), 'kosha.txt')
  await page.reload()
  await waitFor(async () => (await page.locator('#status').textContent()) === 'Подключено', 'short URL refresh')
  assert.equal(page.url(), shortUrl)
  assert.equal(await page.locator('#language').inputValue(), 'text')
  await page.click('#new-room')
  await waitFor(() => page.url() !== shortUrl && /^\/[A-Z0-9]{6}$/.test(new URL(page.url()).pathname), 'new short room')
  assert.equal((await page.locator('.cm-content').textContent())?.trim(), '', 'new room must be empty')

  // Preserve the old fragment-based room names and their document identity.
  const url = `${server.url}/#browser-smoke`
  await Promise.all([page.goto(url), peer.goto(url)])
  await waitFor(async () => (await page.locator('#status').textContent()) === 'Подключено' && (await peer.locator('#status').textContent()) === 'Подключено', 'browser clients connect')
  const editorText = p => p.locator('.cm-line').evaluateAll(lines => lines.map(line => {
    const copy = line.cloneNode(true)
    // Presence labels are inline widgets, not part of the shared document.
    for (const caret of copy.querySelectorAll('.cm-ySelectionCaret')) caret.remove()
    return copy.textContent || ''
  }).join('\n'))
  await page.locator('.cm-content').click()
  await page.keyboard.insertText('def greet(name):\n    return "Привет 👋, " + name\n')
  await waitFor(async () => (await editorText(peer)).includes('def greet(name)'), 'browser editor sync')
  assert(await page.locator('.cm-line span').filter({ hasText: /^def$/ }).count() > 0, 'Python keyword must have a syntax span')
  await peer.locator('.cm-content').click()
  await peer.keyboard.press('ControlOrMeta+End')
  await peer.keyboard.insertText('# peer edit\n')
  await waitFor(async () => (await editorText(page)).includes('# peer edit'), 'remote browser edits')
  await page.locator('.cm-content').click()
  await page.keyboard.press('ControlOrMeta+z')
  try { await waitFor(async () => (await editorText(page)) === (await editorText(peer)), 'undo propagates') }
  catch (error) { throw new Error(`${error.message}\n${JSON.stringify({ local: await editorText(page), remote: await editorText(peer), errors })}`) }
  assert((await editorText(page)).includes('# peer edit') && !(await editorText(page)).includes('def greet'), 'undo must remove only local edits and preserve the remote edit')
  await page.keyboard.press('ControlOrMeta+Shift+z')
  await waitFor(async () => (await editorText(peer)).includes('def greet'), 'redo propagates')
  assert((await editorText(peer)).includes('# peer edit'), 'redo must preserve the remote edit')
  await page.selectOption('#language', 'sql')
  await waitFor(async () => (await peer.locator('#language').inputValue()) === 'sql', 'shared SQL selection')
  await page.locator('.cm-content').click()
  await page.keyboard.press('ControlOrMeta+a')
  await page.keyboard.insertText('SELECT name, count(*)\nFROM participants\nGROUP BY name;\n')
  await waitFor(async () => (await editorText(peer)).startsWith('SELECT name'), 'SQL editor sync')
  assert(await page.locator('.cm-line span').filter({ hasText: /^SELECT$/ }).count() > 0, 'SQL keyword must have a syntax span')
  await page.reload()
  await waitFor(async () => (await page.locator('#status').textContent()) === 'Подключено' && (await editorText(page)).includes('FROM participants'), 'browser refresh recovery')
  assert.equal(await page.locator('#language').inputValue(), 'sql')
  await page.screenshot({ path: 'artifacts/browser-desktop.png', fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'mobile page must fit viewport')
  await page.screenshot({ path: 'artifacts/browser-mobile.png', fullPage: true })
  assert.deepEqual(errors, [], 'no browser JS errors')
  assert.deepEqual(external, [], 'all page resources must use the application origin')
  // Keep a real editor open across a server outage longer than the room TTL.
  const retentionDatabase = join(dir, 'retention.db')
  let retentionServer = await startServer(retentionDatabase, ['-room-ttl', '2s'])
  const staleContext = await browser.newContext({ acceptDownloads: true })
  try {
    const stale = await staleContext.newPage()
    const retentionErrors = []
    stale.on('pageerror', error => retentionErrors.push(String(error)))
    await stale.goto(retentionServer.url)
    await waitFor(async () => (await stale.locator('#status').textContent()) === 'Подключено', 'retention browser connects')
    const expiredUrl = stale.url(), oldId = new URL(expiredUrl).pathname.slice(1)
    await stale.locator('.cm-content').click()
    await stale.keyboard.insertText('saved before restart\n')
    const address = new URL(retentionServer.url).host
    await retentionServer.stop()
    await stale.keyboard.insertText('offline draft preserved 🐾\n')
    await sleep(2200)
    retentionServer = await startServer(retentionDatabase, ['-room-ttl', '2s', '-addr', address])
    await waitFor(async () => (await stale.locator('#status').textContent()) === 'Срок хранения комнаты истёк', 'expired stale tab stops reconnecting')
    assert((await editorText(stale)).includes('offline draft preserved 🐾'))
    assert.equal(await stale.locator('.cm-content').getAttribute('contenteditable'), 'false')
    const [preserved] = await Promise.all([stale.waitForEvent('download'), stale.click('#download')])
    const preservedText = await readFile(await preserved.path(), 'utf8')
    assert(preservedText.includes('saved before restart') && preservedText.includes('offline draft preserved 🐾'))
    const expiredPage = await stale.reload()
    assert.equal(expiredPage.status(), 410)
    assert((await stale.locator('h1').textContent()).includes('Срок хранения'))
    await stale.goto(`${retentionServer.url}/#${oldId}`)
    await waitFor(async () => (await stale.locator('#status').textContent()) === 'Срок хранения комнаты истёк', 'expired legacy page is explicit')
    await stale.click('#new-room')
    await waitFor(async () => (await stale.locator('#status').textContent()) === 'Подключено', 'new room after expiration')
    assert.notEqual(new URL(stale.url()).pathname, `/${oldId}`)
    assert.deepEqual(retentionErrors, [], 'expiration is a normal UI state, without JS exceptions')
    console.log('PASS: expired browser tab keeps offline draft downloadable, freezes editing, reports 410 and creates a fresh room')
  } finally {
    try { await staleContext.close() }
    finally { await retentionServer.stop() }
  }
  console.log('PASS: Text and .txt download, short room URLs/reload/new room, legacy links, Python/SQL highlighting, local-only undo/redo, shared language, mobile layout, same-origin resources')
} finally {
  try { if (browser) await browser.close() }
  finally { await server.stop() }
}
