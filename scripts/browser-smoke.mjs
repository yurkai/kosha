import assert from 'node:assert/strict'
import { mkdir, mkdtemp } from 'node:fs/promises'
import { join, resolve } from 'node:path'
import { chromium } from 'playwright'
import { startServer, waitFor } from './lib/harness.mjs'

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
  console.log('PASS: two isolated browser contexts, Python/SQL highlighting, editing, local-only undo/redo, shared language, refresh, mobile layout, same-origin resources')
} finally {
  try { if (browser) await browser.close() }
  finally { await server.stop() }
}
