import { EditorView, drawSelection, highlightActiveLine, keymap, lineNumbers } from '@codemirror/view'
import { Compartment } from '@codemirror/state'
import { defaultKeymap, indentWithTab } from '@codemirror/commands'
import { bracketMatching, defaultHighlightStyle, indentOnInput, syntaxHighlighting } from '@codemirror/language'
import { highlightSelectionMatches, searchKeymap } from '@codemirror/search'
import { python } from '@codemirror/lang-python'
import { sql } from '@codemirror/lang-sql'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'
import { yCollab, yUndoManagerKeymap } from 'y-codemirror.next'
import './style.css'

const element = <T extends HTMLElement>(id: string) => document.getElementById(id) as T
const roomId = () => Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('')
const room = location.hash.slice(1) || roomId()
if (!/^[A-Za-z0-9_-]{1,64}$/.test(room)) {
  element('status').textContent = 'Неверная ссылка комнаты'
  throw new Error('Invalid room name')
}
if (!location.hash) history.replaceState(null, '', `#${room}`)
window.addEventListener('hashchange', () => location.reload())

const doc = new Y.Doc()
const text = doc.getText('code')
const settings = doc.getMap<string>('settings')
const socketUrl = `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/yjs`
const provider = new WebsocketProvider(socketUrl, room, doc, { disableBc: true })
const syntax = new Compartment()
const language = element<HTMLSelectElement>('language')
const name = element<HTMLInputElement>('name')
name.value = `Гость ${doc.clientID.toString().slice(-4)}`
const color = ['#4969ba', '#b14c74', '#258674', '#a46a29'][doc.clientID % 4]
const setUser = () => provider.awareness.setLocalStateField('user', {
  name: name.value.trim() || 'Гость', color, colorLight: `${color}22`,
})
setUser()
name.addEventListener('input', setUser)

const view = new EditorView({
  parent: element('editor'),
  extensions: [
    lineNumbers(), drawSelection(), highlightActiveLine(), highlightSelectionMatches(),
    bracketMatching(), indentOnInput(), syntaxHighlighting(defaultHighlightStyle),
    // Yjs owns history. CodeMirror's regular history also records remote changes.
    keymap.of([...yUndoManagerKeymap, ...defaultKeymap, ...searchKeymap, indentWithTab]),
    syntax.of(python()),
    yCollab(text, provider.awareness, { undoManager: new Y.UndoManager(text) }),
    EditorView.theme({ '&': { height: '100%' }, '.cm-scroller': { overflow: 'auto' } }),
    EditorView.contentAttributes.of({ 'aria-label': 'Код', spellcheck: 'false' }),
  ],
})

const updateLanguage = () => {
  language.value = settings.get('language') === 'sql' ? 'sql' : 'python'
  view.dispatch({ effects: syntax.reconfigure(language.value === 'sql' ? sql() : python()) })
}
settings.observe(updateLanguage)
language.addEventListener('change', () => settings.set('language', language.value))
updateLanguage()

const updateStatus = () => {
  const connected = provider.wsconnected && provider.synced
  element('status').textContent = connected ? 'Подключено' : provider.wsconnected ? 'Синхронизация…' : 'Нет связи · правки пока в браузере'
  element('connection-dot').classList.toggle('online', connected)
}
provider.on('status', updateStatus)
provider.on('sync', updateStatus)
provider.awareness.on('change', () => {
  element('participants').textContent = `В комнате: ${provider.awareness.getStates().size}`
})

const notice = (message: string) => { element('notice').textContent = message }
element('share').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText(location.href); notice('Ссылка скопирована. Все, у кого она есть, могут редактировать.') }
  catch { notice('Скопируйте ссылку из адресной строки.') }
})
element('new-room').addEventListener('click', () => {
  if (!provider.synced && text.length && !confirm('Есть правки без соединения. Скачайте текст перед переходом. Всё равно открыть новую комнату?')) return
  location.hash = roomId()
})
element('download').addEventListener('click', () => {
  const url = URL.createObjectURL(new Blob([text.toString()], { type: 'text/plain;charset=utf-8' }))
  const a = document.createElement('a')
  a.href = url
  a.download = `kosha.${language.value === 'sql' ? 'sql' : 'py'}`
  a.click()
  URL.revokeObjectURL(url)
})
window.addEventListener('pagehide', () => { provider.destroy(); view.destroy(); doc.destroy() })
