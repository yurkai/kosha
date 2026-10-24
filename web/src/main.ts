import { EditorView, drawSelection, highlightActiveLine, keymap, lineNumbers } from '@codemirror/view'
import { Compartment, StateEffect } from '@codemirror/state'
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
const createRoom = async () => {
  const response = await fetch('/api/rooms', { method: 'POST' })
  if (!response.ok) throw new Error('Не удалось создать комнату. Попробуйте ещё раз.')
  const room: { id: string } = await response.json()
  if (!/^[A-Z0-9]{6}$/.test(room.id)) throw new Error('Сервер вернул неверную ссылку комнаты.')
  return room.id
}
const legacyRoom = location.pathname === '/' ? location.hash.slice(1) : ''
let room = legacyRoom || location.pathname.slice(1)
if (!room) {
  try {
    room = await createRoom()
    history.replaceState(null, '', `/${room}`)
  } catch (error) {
    element('status').textContent = 'Не удалось создать комнату. Обновите страницу.'
    throw error
  }
}
if (!/^[A-Za-z0-9_-]{1,64}$/.test(room)) {
  element('status').textContent = 'Неверная ссылка комнаты'
  throw new Error('Invalid room name')
}
window.addEventListener('hashchange', () => location.reload())

// Fragments never reach the HTTP server. This marks one actual page opening,
// independently of automatic WebSocket reconnects and status probes.
const opening = await fetch(`/api/rooms/${encodeURIComponent(room)}/visit`, { method: 'POST' }).catch(() => null)
if (opening?.ok) startEditor(room)
else showUnavailable(opening?.status === 410)

function showUnavailable(expired: boolean) {
  element('status').textContent = expired ? 'Срок хранения комнаты истёк' : 'Комната недоступна'
  element('notice').textContent = expired
    ? 'Документ удалён после периода неактивности. Создайте новую комнату.'
    : 'Проверьте соединение и обновите страницу.'
  for (const id of ['download', 'share', 'language', 'name']) element<HTMLInputElement | HTMLButtonElement>(id).disabled = true
  element('new-room').addEventListener('click', () => location.assign('/'))
}

function startEditor(room: string) {
  const doc = new Y.Doc()
  const text = doc.getText('code')
  const settings = doc.getMap<string>('settings')
  const socketUrl = `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/yjs`
  const provider = new WebsocketProvider(socketUrl, room, doc, { disableBc: true })
  let expired = false, closed = false, checkingRoom = false
  const syntax = new Compartment()
  const language = element<HTMLSelectElement>('language')
  const modes = {
    python: { syntax: python, extension: 'py' },
    sql: { syntax: sql, extension: 'sql' },
    text: { syntax: () => [], extension: 'txt' },
  }
  const selectedMode = () => {
    const current = settings.get('language')
    return current === 'sql' || current === 'text' ? current : 'python'
  }
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
      EditorView.contentAttributes.of({ 'aria-label': 'Текст документа', spellcheck: 'false' }),
    ],
  })

  const updateLanguage = () => {
    language.value = selectedMode()
    view.dispatch({ effects: syntax.reconfigure(modes[selectedMode()].syntax()) })
  }
  settings.observe(updateLanguage)
  language.addEventListener('change', () => settings.set('language', language.value))
  updateLanguage()

  const updateStatus = () => {
    if (expired) return
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
  const checkAvailability = async () => {
    if (expired || closed || checkingRoom) return
    checkingRoom = true
    try {
      // A diagnostic read must never extend the room's retention period.
      const response = await fetch(`/api/rooms/${encodeURIComponent(room)}`)
      if (!closed && response.status === 410) {
        expired = true
        provider.disconnect()
        view.dispatch({ effects: StateEffect.appendConfig.of(EditorView.editable.of(false)) })
        element('status').textContent = 'Срок хранения комнаты истёк'
        element('connection-dot').classList.remove('online')
      language.disabled = true
        notice('Комната удалена. Скачайте оставшийся в этой вкладке текст и создайте новую комнату.')
      }
    } catch { /* A failed probe during an outage does not imply expiration. */ }
    finally { checkingRoom = false }
  }
  provider.on('connection-close', checkAvailability)
  provider.on('connection-error', checkAvailability)
  element('share').addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(location.href); notice('Ссылка скопирована. Все, у кого она есть, могут редактировать.') }
    catch { notice('Скопируйте ссылку из адресной строки.') }
  })
  element<HTMLButtonElement>('new-room').addEventListener('click', async () => {
    if (!provider.synced && text.length && !confirm('Есть правки без соединения. Скачайте текст перед переходом. Всё равно открыть новую комнату?')) return
    const button = element<HTMLButtonElement>('new-room')
    button.disabled = true
    try { location.assign(`/${await createRoom()}`) }
    catch { notice('Не удалось создать комнату. Попробуйте ещё раз.'); button.disabled = false }
  })
  element('download').addEventListener('click', () => {
    const url = URL.createObjectURL(new Blob([text.toString()], { type: 'text/plain;charset=utf-8' }))
    const a = document.createElement('a')
    a.href = url
    a.download = `kosha.${modes[selectedMode()].extension}`
    a.click()
    URL.revokeObjectURL(url)
  })
  window.addEventListener('pagehide', () => { closed = true; provider.destroy(); view.destroy(); doc.destroy() })
}
