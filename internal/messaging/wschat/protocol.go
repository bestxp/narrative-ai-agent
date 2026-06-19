// Package wschat implements the browser WebSocket chat transport.
//
// The transport serves an embedded React single-page app on "/" and
// a WebSocket endpoint on "/ws". The wire protocol is JSON envelopes
// with a "type" discriminator so new message kinds can be added
// without breaking older clients. Authentication is a bearer token
// validated on the WebSocket upgrade and on HTTP API calls; in dev
// the single dev_token from config is accepted, in production the
// allowed_tokens list is the hook for per-user tokens.
//
// The client implements messaging.Client so the bot core treats it
// the same as Telegram or VK. Streaming (word-by-word LLM output)
// maps onto WS frames of type "delta" / "final"; the chat history
// the browser keeps is driven by "message" frames for completed
// turns.
package wschat

import "encoding/json"

// Frame is the top-level JSON envelope exchanged over the WebSocket.
// Every frame carries a type tag; the payload shape depends on the
// type. The protocol is designed to be forward-compatible: unknown
// types MUST be ignored by the client so a newer server can emit
// new frame kinds without breaking older UIs.
type Frame struct {
	// Type identifies the frame. See the Frame* constants.
	Type string `json:"type"`
	// Payload is the type-specific JSON body. Clients unmarshal it
	// lazily based on Type.
	Payload json.RawMessage `json:"payload,omitempty"`
	// ID is an optional correlation id echoed back by the server
	// on response frames (e.g. edit_last / resend_last). The client
	// generates it; the server copies it onto the matching reply.
	ID string `json:"id,omitempty"`
	// V is an optional protocol version. Today it is always 1;
	// future incompatible changes bump it so a client can refuse
	// to talk to a server it does not understand.
	V int `json:"v,omitempty"`
}

// Frame types. The set is split into client→server (the actions the
// browser issues) and server→client (the events the server pushes
// back). Unknown types MUST be ignored.
const (
	// Client→server: send a freeform message or a /command. Payload
	// is SendPayload.
	FrameSend = "send"
	// Client→server: edit the last user message and regenerate the
	// LLM answer. Payload is EditPayload.
	FrameEditLast = "edit_last"
	// Client→server: resend the last user message (discard the last
	// LLM answer and generate a fresh one). Payload is empty.
	FrameResendLast = "resend_last"
	// Client→server: request the command list. The server replies
	// with a FrameCommandList. Payload is empty.
	FrameCommandListRequest = "command_list_request"

	// Server→client: ack for a send / edit / resend. Payload is
	// AckPayload. The ID matches the originating client frame.
	FrameAck = "ack"
	// Server→client: an error for a previously-acked operation.
	// Payload is ErrorPayload. The ID matches the originating frame
	// when applicable.
	FrameError = "error"
	// Server→client: a chat message (user or assistant) that is now
	// considered final. Payload is MessagePayload.
	FrameMessage = "message"
	// Server→client: a streaming delta — partial assistant text. The
	// client replaces the in-progress assistant bubble's text with
	// the payload text (not append). Payload is DeltaPayload.
	FrameDelta = "delta"
	// Server→client: a status rotation ("…принял", "…собираю
	// контекст"). Payload is StatusPayload.
	FrameStatus = "status"
	// Server→client: the command list reply. Payload is
	// CommandListPayload.
	FrameCommandList = "command_list"
)

// SendPayload is the body of a FrameSend. Either Text or Command
// must be set, not both. When Command is non-empty, Text is ignored
// and the message is dispatched as a slash command (no LLM round).
type SendPayload struct {
	// Text is the freeform user message. When empty and Command is
	// set, the frame is a command invocation.
	Text string `json:"text,omitempty"`
	// Command is the slash command without the leading "/", e.g.
	// "me", "status", "launch". When set, Text is ignored.
	Command string `json:"command,omitempty"`
	// Args is the optional argument list for a command invocation.
	Args []string `json:"args,omitempty"`
}

// EditPayload is the body of FrameEditLast. The server replaces the
// last user message text with NewText and regenerates the LLM answer.
type EditPayload struct {
	NewText string `json:"new_text"`
}

// AckPayload is the body of FrameAck. OK is true when the operation
// was accepted; when false, Message carries a short reason.
type AckPayload struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// ErrorPayload is the body of FrameError.
type ErrorPayload struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// MessagePayload is the body of FrameMessage. It represents one
// chat message that is now considered final (not streaming).
type MessagePayload struct {
	// Role is "user" or "assistant".
	Role string `json:"role"`
	// Text is the final message body (markdown for assistant).
	Text string `json:"text"`
	// Command is set when Role=="user" and the message was a slash
	// command invocation. Empty for freeform user messages and for
	// all assistant messages.
	Command string `json:"command,omitempty"`
	// Tokens is the cumulative token usage for the assistant turn.
	// nil when Role=="user" or when token tracking is off — the
	// pointer (not value) lets json:",omitempty" actually drop the
	// field. Mirrors llm.Usage; duplicated here so the protocol
	// package does not depend on the LLM driver.
	Tokens *TokenUsage `json:"tokens,omitempty"`
}

// TokenUsage is the wire shape of llm.Usage as it travels over the
// WebSocket. The Source field is "off"|"estimate"|"usage" depending
// on which tracking mode produced the count.
type TokenUsage struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Source           string `json:"source,omitempty"`
}

// DeltaPayload is the body of FrameDelta. The text is the FULL
// current assistant buffer (the client replaces, not appends).
type DeltaPayload struct {
	Text string `json:"text"`
}

// StatusPayload is the body of FrameStatus.
type StatusPayload struct {
	Phase   string         `json:"phase"`
	Details map[string]any `json:"details,omitempty"`
}

// CommandListPayload is the body of FrameCommandList.
type CommandListPayload struct {
	Commands []CommandDesc `json:"commands"`
}

// CommandDesc is one entry in the command list. Mirror of
// messaging.BotCommand but kept independent so the protocol package
// does not depend on the messaging package (and vice versa).
type CommandDesc struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}
