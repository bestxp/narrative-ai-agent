import React, { useState, useEffect, useRef, useCallback, useMemo } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

// wschat — single-window dev chat for the narrative bot.
//
// Connection: opens a WebSocket to /ws?token=<dev_token>. The token
// is read from the URL query (?token=...) so an operator can share
// a bookmarked link; when absent we fall back to the
// VITE_WSCHAT_TOKEN env at build time, and finally to a hard-coded
// dev default. The Go server checks the same token on upgrade.
const DEFAULT_TOKEN = 'dev-secret-change-me'

function getToken() {
  const fromURL = new URLSearchParams(window.location.search).get('token')
  if (fromURL) return fromURL
  if (import.meta.env.VITE_WSCHAT_TOKEN) return import.meta.env.VITE_WSCHAT_TOKEN
  return DEFAULT_TOKEN
}

// nextId returns a short monotonic id used both for client→server
// frame correlation and for keys in the local message list. The
// server echoes the id on every reply frame so the client can group
// them into a single turn.
let _idCounter = 0
function nextId() { _idCounter += 1; return String(_idCounter) }

// formatTokens renders a token footer under the assistant bubble.
// source: "estimate" | "usage" | "" (tracking off). An empty object
// yields null so the footer disappears cleanly.
function formatTokens(t) {
  if (!t || !t.source || t.source === 'off') return null
  if (!t.total_tokens) return null
  return `🔢 ${t.total_tokens.toLocaleString('ru-RU')} tok (${t.source === 'estimate' ? 'оценка' : 'api'})`
}

export default function App() {
  // The message list is the single source of truth. Every entry
  // carries a stable id (== the client→server frame id) and a
  // turnId (== id, since we own the turn on the client). Server
  // frames carry the same id back, and the reducer keys on it so
  // replace-by-turnId is a single operation.
  const [messages, setMessages] = useState([])
  const [draft, setDraft] = useState('')
  const [connected, setConnected] = useState(false)
  const [commands, setCommands] = useState([])
  const [editingId, setEditingId] = useState(null) // message id being edited
  const [editText, setEditText] = useState('')
  // streamingByTurn mirrors the placeholder text for each active
  // turn so delta frames can update without going through the
  // whole messages array. The keys are turn ids.
  const streamingRef = useRef({})
  const [, forceUpdate] = useState(0)
  const tick = useCallback(() => forceUpdate(n => n + 1), [])

  const wsRef = useRef(null)
  const messagesEndRef = useRef(null)
  const messagesContainerRef = useRef(null)
  // pinnedToBottom: true when the user is at (or near) the bottom
  // of the messages list. When false, auto-scroll is suspended so
  // reading an older message does not get interrupted by a fresh
  // delta landing somewhere in the middle. The user can re-pin by
  // hitting the "↓" jump button.
  const pinnedToBottomRef = useRef(true)
  // Latest streaming placeholder key — we use it to scroll the
  // bubble into view when a turn starts (the placeholders are
  // appended below the messages list, so without an explicit
  // scroll the bubble may end up off-screen on a long history).
  const latestStreamTurnRef = useRef(null)
  const latestStreamPlaceholderRef = useRef(null)
  const [jumpToBottom, setJumpToBottom] = useState(false)

  // Track scroll position: mark "pinned to bottom" when the user
  // is within 80px of the bottom edge, otherwise mark them as
  // reading older content.
  const onMessagesScroll = useCallback(() => {
    const el = messagesContainerRef.current
    if (!el) return
    const dist = el.scrollHeight - el.scrollTop - el.clientHeight
    const pinned = dist <= 80
    pinnedToBottomRef.current = pinned
    if (pinned && jumpToBottom) setJumpToBottom(false)
  }, [jumpToBottom])

  // Scroll to the bottom of the messages container. Called when a
  // new turn starts (placeholder appears) or when the user hits
  // the "↓" jump button.
  const scrollToBottom = useCallback(() => {
    const el = messagesContainerRef.current
    if (!el) return
    el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
    pinnedToBottomRef.current = true
    setJumpToBottom(false)
  }, [])

  // Scroll whenever the message list grows or streaming state
  // changes. Behaviour depends on whether the user is pinned to
  // the bottom; when they are, we always scroll the new content
  // into view; when they are not, we surface a "jump to latest"
  // pill instead of forcing the scroll.
  useEffect(() => {
    if (pinnedToBottomRef.current) {
      scrollToBottom()
    } else {
      setJumpToBottom(true)
    }
  }, [messages, streamingRef.current, scrollToBottom])

  // WebSocket setup.
  useEffect(() => {
    const token = getToken()
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const url = `${proto}//${window.location.host}/ws?token=${encodeURIComponent(token)}`
    const ws = new WebSocket(url)
    wsRef.current = ws

    ws.onopen = () => setConnected(true)
    ws.onclose = () => { setConnected(false); wsRef.current = null }
    ws.onerror = () => setConnected(false)

    ws.onmessage = (event) => {
      let frame
      try { frame = JSON.parse(event.data) } catch { return }
      handleFrame(frame)
    }

    return () => { ws.close() }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Fetch the command list once the socket is connected.
  useEffect(() => {
    const token = getToken()
    fetch(`/api/commands`, { headers: { Authorization: `Bearer ${token}` } })
      .then(r => r.ok ? r.json() : Promise.reject(r.status))
      .then(data => setCommands(data.commands || []))
      .catch(() => { /* server not ready yet; will retry via WS */ })
  }, [connected])

  // handleFrame is the single inbound reducer. Every server frame
  // carries a turn id; we look up the message with that id and
  // either patch it in place or append a new one. No optimistic
  // updates on the client: the user message bubble is created only
  // when the server echoes it back, which guarantees one bubble
  // per turn regardless of the operation (send/edit/resend).
  const handleFrame = useCallback((frame) => {
    const p = frame.payload && typeof frame.payload === 'object'
      ? frame.payload
      : (frame.payload ? JSON.parse(frame.payload) : {})
    const turnId = frame.id || ''

    switch (frame.type) {
      case 'message': {
        const role = p.role
        const text = p.text || ''
        const tokens = p.tokens
        setMessages(prev => {
          // Replace-by-turnId: if a message with the same turnId
          // already exists, update its content. Otherwise append.
          // Covers three cases at once:
          //   - send: turnId is fresh, message is appended
          //   - edit: server echoes user with the same turnId
          //     that was used for the original send (but the
          //     edit frame itself has a new id — see below)
          //   - resend: server echoes user with a fresh turnId
          //     for the regeneration
          const idx = prev.findIndex(m => m.turnId === turnId && m.role === role)
          if (idx >= 0) {
            const next = prev.slice()
            next[idx] = { ...next[idx], text, tokens, streaming: false }
            return next
          }
          return [...prev, {
            id: turnId || nextId(),
            turnId,
            role,
            text,
            command: p.command || '',
            tokens,
            streaming: false,
          }]
        })
        // Clear streaming placeholder for this turn (it has now
        // arrived as a final message).
        if (streamingRef.current[turnId]) {
          delete streamingRef.current[turnId]
        }
        // Reset the latest-stream pointer so the next turn can
        // re-mark itself as latest and trigger the auto-scroll
        // "open" effect.
        if (latestStreamTurnRef.current === turnId) {
          latestStreamTurnRef.current = null
        }
        tick()
        break
      }
      case 'delta': {
        const text = p.text || ''
        streamingRef.current[turnId] = { kind: 'delta', text }
        latestStreamTurnRef.current = turnId
        tick()
        break
      }
      case 'status': {
        // Status frames replace the placeholder until the first
        // delta arrives for this turn. The first status is also
        // what "opens" the turn visually — mark this turn as the
        // latest streaming one so the auto-scroll effect can move
        // the placeholder into view.
        streamingRef.current[turnId] = { kind: 'status', phase: p.phase, text: statusLabel(p.phase) }
        if (!latestStreamTurnRef.current) {
          latestStreamTurnRef.current = turnId
        }
        tick()
        break
      }
      case 'command_list': {
        setCommands(p.commands || [])
        break
      }
      case 'error': {
        // Errors NEVER replace an existing message — they are
        // assistant-side meta bubbles, surfaced so the operator
        // sees something went wrong. We pin each error to a
        // synthetic turn id so it gets its own row; the original
        // user message (echoed earlier under the real turn id)
        // stays put so the operator can hit "resend" again.
        const syntheticId = `err-${nextId()}`
        const text = `⚠️ ${p.message}${p.code ? ` (${p.code})` : ''}`
        setMessages(prev => {
          const idx = prev.findIndex(m => m.turnId === syntheticId)
          if (idx >= 0) {
            const next = prev.slice()
            next[idx] = { ...next[idx], text, streaming: false }
            return next
          }
          return [...prev, { id: syntheticId, turnId: syntheticId, role: 'assistant', text, streaming: false }]
        })
        if (streamingRef.current[turnId]) {
          delete streamingRef.current[turnId]
        }
        tick()
        break
      }
      case 'ack': {
        // No-op; the matching message frame will arrive shortly
        // and create the user bubble.
        break
      }
      default:
        // Unknown frames are ignored (forward-compat).
    }
  }, [tick])

  // sendFrame sends a client→server frame. payload is a plain
  // object (NOT a JSON string) so the server's json.RawMessage
  // field receives a JSON object it can unmarshal.
  const sendFrame = useCallback((type, payload) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    const id = nextId()
    const frame = { type, id, v: 1 }
    if (payload !== undefined) frame.payload = payload
    ws.send(JSON.stringify(frame))
    return id
  }, [])

  // Send a freeform message.
  const sendText = useCallback((text) => {
    const trimmed = text.trim()
    if (!trimmed) return
    sendFrame('send', { text: trimmed })
    setDraft('')
  }, [sendFrame])

  // Send a slash command.
  const sendCommand = useCallback((cmd, args = []) => {
    sendFrame('send', { command: cmd, args })
  }, [sendFrame])

  // Edit the last user message: enter edit mode.
  const startEditLast = useCallback(() => {
    // Find the last user message in the list.
    const lastUser = [...messages].reverse().find(m => m.role === 'user')
    if (!lastUser) return
    setEditingId(lastUser.id)
    // Strip leading "/" when the message is a command, so the
    // operator can edit the raw command line.
    setEditText(lastUser.command ? lastUser.text : lastUser.text)
  }, [messages])

  // Confirm edit: send edit_last with the new text.
  const confirmEdit = useCallback(() => {
    const trimmed = editText.trim()
    if (!trimmed) return
    // Optimistically patch the local user bubble so the UI snaps
    // to the new text before the server echo arrives. The echo
    // will reconcile by turnId (a different turnId — but our
    // optimistic patch is already correct, and the echo's
    // replace-by-turnId will harmlessly overwrite it with the
    // same text).
    setMessages(prev => {
      const next = prev.slice()
      for (let i = next.length - 1; i >= 0; i--) {
        if (next[i].role === 'user') {
          next[i] = { ...next[i], text: trimmed }
          break
        }
      }
      // Drop trailing assistant bubbles from the prior turn so
      // the UI shows a streaming placeholder once the echo + new
      // assistant arrive.
      while (next.length && next[next.length - 1].role === 'assistant') {
        next.pop()
      }
      return next
    })
    sendFrame('edit_last', { new_text: trimmed })
    setEditingId(null)
    setEditText('')
  }, [editText, sendFrame])

  // Cancel edit mode.
  const cancelEdit = useCallback(() => {
    setEditingId(null)
    setEditText('')
  }, [])

  // Resend the last user message: regenerate the LLM answer.
  const resendLast = useCallback(() => {
    // Drop trailing assistant bubbles; the new assistant will
    // arrive as a streaming placeholder under the existing user.
    setMessages(prev => {
      const next = prev.slice()
      while (next.length && next[next.length - 1].role === 'assistant') {
        next.pop()
      }
      return next
    })
    sendFrame('resend_last')
  }, [sendFrame])

  // Keyboard: Enter to send, Shift+Enter for newline.
  const onKeyDown = (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      if (editingId) confirmEdit()
      else sendText(draft)
    }
  }

  // Find the last user message id — that's the one whose
  // "edit / resend" row we render.
  const lastUserId = useMemo(() => {
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i].role === 'user') return messages[i].id
    }
    return null
  }, [messages])

  return (
    <div className="app">
      <div className="header">
        <h1>wschat</h1>
        <span className={`status ${connected ? 'connected' : 'disconnected'}`}>
          {connected ? '● online' : '○ offline'}
        </span>
      </div>

      {commands.length > 0 && (
        <div className="command-bar">
          {commands.map(c => (
            <button key={c.command} onClick={() => sendCommand(c.command)} title={c.description}>
              /{c.command}<span className="desc">{c.description}</span>
            </button>
          ))}
        </div>
      )}

      <div className="messages" ref={messagesContainerRef} onScroll={onMessagesScroll}>
        {messages.map(m => (
          <React.Fragment key={m.id}>
            <MessageBubble msg={m} />
            {m.role === 'user' && m.id === lastUserId && !editingId && (
              <div className="msg-actions-row">
                <button onClick={startEditLast} title="Редактировать и переотправить">✎ edit</button>
                <button onClick={resendLast} title="Переотправить (новый ответ)">↻ resend</button>
              </div>
            )}
          </React.Fragment>
        ))}
        {/* Streaming placeholders, one per active turn. Keyed by
            turnId so a fresh turn does not collide with an older
            one that is still streaming. We hold a ref on the
            latest placeholder so the auto-scroll effect can move
            it into view the moment the first delta lands. */}
        {Object.entries(streamingRef.current).map(([turnId, info]) => {
          const isLatest = latestStreamTurnRef.current === turnId
          return (
            <div
              key={`stream-${turnId}`}
              ref={isLatest ? latestStreamPlaceholderRef : null}
              className="msg assistant streaming-bubble"
            >
              {info.kind === 'status'
                ? <span className="status-text">{info.text}</span>
                : <ReactMarkdown remarkPlugins={[remarkGfm]}>{info.text}</ReactMarkdown>}
            </div>
          )
        })}
        <div ref={messagesEndRef} />
      </div>

      {jumpToBottom && (
        <button className="jump-to-bottom" onClick={scrollToBottom} title="К последнему сообщению">
          ↓ к новому
        </button>
      )}

      {editingId && (
        <div className="edit-banner">
          <span>Редактирование последнего сообщения — Enter чтобы отправить, Esc чтобы отменить</span>
          <button onClick={cancelEdit}>отмена</button>
        </div>
      )}

      <div className="composer-wrap">
        <div className="composer">
          <textarea
            value={editingId ? editText : draft}
            onChange={e => editingId ? setEditText(e.target.value) : setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape' && editingId) { e.preventDefault(); cancelEdit(); return }
              onKeyDown(e)
            }}
            placeholder={editingId
              ? 'Отредактируйте сообщение и нажмите Enter…'
              : 'Сообщение… (Enter — отправить, Shift+Enter — новая строка)'}
            rows={2}
            autoFocus
          />
          <button
            onClick={() => editingId ? confirmEdit() : sendText(draft)}
            disabled={editingId ? !editText.trim() : !draft.trim()}
          >
            {editingId ? '↵' : '➤'}
          </button>
        </div>
      </div>
    </div>
  )
}

// MessageBubble renders one final (non-streaming) message. For
// assistant bubbles it also shows a token footer when usage data
// is present. The action row (edit / resend) is rendered by the
// parent as a separate sibling so it can live OUTSIDE the bubble
// and stay anchored to the user message it belongs to.
function MessageBubble({ msg }) {
  return (
    <div className={`msg ${msg.role}`}>
      {msg.role === 'assistant'
        ? <ReactMarkdown remarkPlugins={[remarkGfm]}>{msg.text}</ReactMarkdown>
        : <span>{msg.text}</span>}
      {msg.role === 'assistant' && (() => {
        const footer = formatTokens(msg.tokens)
        return footer ? <div className="tokens">{footer}</div> : null
      })()}
    </div>
  )
}

// statusLabel maps a GM phase label to a human-readable Russian
// string shown in the streaming placeholder.
function statusLabel(phase) {
  switch (phase) {
    case 'request_received': return 'принял…'
    case 'build_context': return 'собираю контекст…'
    case 'llm_request': return 'спрашиваю модель…'
    case 'tool_dispatch': return 'применяю инструменты…'
    default: return '…'
  }
}