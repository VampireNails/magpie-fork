package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

func failureSecrets(p provider.Provider, in http.Header) []string {
	secrets := []string{p.Key, p.SecretAccessKey}
	for _, candidate := range append(provider.All(), p) {
		secrets = append(secrets, candidate.Key, candidate.SecretAccessKey)
		for _, key := range candidate.Keys {
			secrets = append(secrets, key.Key)
		}
	}
	auth := strings.TrimSpace(in.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		auth = auth[7:]
	}
	return append(secrets, auth, in.Get("x-api-key"), in.Get("x-goog-api-key"))
}

// hideFailureSecrets removes credentials known to this attempt even when a
// vendor returns one in an error. Do not rely on the credential's shape.
func hideFailureSecrets(s string, secrets []string) string {
	marker := ""
	for _, candidate := range []string{"[redacted]", "***", "###", "~~~"} {
		safe := true
		for _, secret := range secrets {
			if secret != "" && strings.Contains(candidate, secret) {
				safe = false
				break
			}
		}
		if safe {
			marker = candidate
			break
		}
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		for _, value := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret), string(encoded[1 : len(encoded)-1])} {
			if value == "" {
				continue
			}
			s = hideOneSecret(s, value, marker)
		}
	}
	return s
}

// hideOneSecret removes a credential from text. A real credential is replaced
// wherever it appears. A one-byte value cannot be told from prose and is
// replaced only where it stands alone: withholding the whole
// message when a character happened to appear lost the vendor's own words
// (#248's refusal advice), and replacing it inside them mangled them
// ("magpie-gro[redacted]").
func hideOneSecret(s, value, marker string) string {
	if len(value) >= 2 {
		return strings.ReplaceAll(s, value, marker)
	}
	var b strings.Builder
	for {
		i := strings.Index(s, value)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		before, after := byte(0), byte(0)
		if i > 0 {
			before = s[i-1]
		}
		if i+len(value) < len(s) {
			after = s[i+len(value)]
		}
		if !wordByte(before) && !wordByte(after) {
			b.WriteString(s[:i])
			b.WriteString(marker)
			s = s[i+len(value):]
			continue
		}
		b.WriteString(s[:i+len(value)])
		s = s[i+len(value):]
	}
}

func wordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// Error replies are normally JSON. Redact its string values without altering
// property names or numeric precision; leave the original bytes when unchanged.
func hideFailureBody(body []byte, secrets []string) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil || !json.Valid(body) {
		return []byte(hideFailureSecrets(string(body), secrets))
	}
	changed := false
	var scrub func(any) any
	scrub = func(v any) any {
		switch v := v.(type) {
		case string:
			out := hideFailureSecrets(v, secrets)
			changed = changed || out != v
			return out
		case json.Number:
			out := hideFailureSecrets(string(v), secrets)
			if out != string(v) {
				changed = true
				return out
			}
			return v
		case []any:
			for i := range v {
				v[i] = scrub(v[i])
			}
		case map[string]any:
			out := make(map[string]any, len(v))
			for k, item := range v {
				name := k
				if !errorProtocolField(k) {
					name = hideFailureSecrets(k, secrets)
				}
				changed = changed || name != k
				if _, exists := out[name]; exists {
					return nil
				}
				if errorProtocolEnum(k, item) {
					out[name] = item
				} else {
					out[name] = scrub(item)
				}
			}
			return out
		}
		return v
	}
	value = scrub(value)
	if !changed {
		return body
	}
	out, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"error":{"message":"Upstream error details withheld"}}`)
	}
	return out
}

// These are protocol field names, not dynamic upstream messages or keys.
// Mutating them makes clients unable to decode the error.
func errorProtocolField(name string) bool {
	switch name {
	case "error", "message", "type", "code", "param", "status", "response", "id", "object", "choices", "delta", "index", "finish_reason", "finishReason", "stop_reason", "stop_details", "stop_sequence", "content_block", "content", "text", "refusal", "usage", "request_id", "candidates", "promptFeedback", "blockReason", "parts", "reason", "incomplete_details", "output", "role",
		"input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "reasoning_tokens", "input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details", "cached_tokens", "output_index", "content_index", "item", "part", "model", "sequence_number":
		return true
	}
	return false
}

func errorProtocolEnum(field string, value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	switch field {
	case "type":
		return errorProtocolEvent(s) || errorProtocolType(s)
	case "stop_reason", "finish_reason", "finishReason", "blockReason", "reason", "code":
		if _, ok := filterReasons[s]; ok {
			return true
		}
		switch s {
		case "error", "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "stop", "length", "tool_calls", "function_call", "STOP", "MAX_TOKENS", "RECITATION", "OTHER":
			return true
		}
	case "status":
		switch s {
		case "queued", "in_progress", "completed", "failed", "incomplete", "cancelled":
			return true
		}
	case "role":
		return s == "assistant" || s == "user" || s == "system" || s == "tool"
	case "object":
		return s == "response" || s == "chat.completion" || s == "chat.completion.chunk"
	}
	return false
}

func errorProtocolType(s string) bool {
	switch s {
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "rate_limit_error", "api_error", "overloaded_error", "billing_error", "server_error", "message", "text", "tool_use", "tool_result", "text_delta", "input_json_delta", "thinking_delta", "signature_delta", "thinking", "redacted_thinking":
		return true
	}
	return false
}

// Exact event names handled by the gateway's supported streaming protocols.
// Unknown names remain dynamic and are checked for credentials.
func errorProtocolEvent(s string) bool {
	switch s {
	case "error", "ping", "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "codex.rate_limits", "codex.response.metadata",
		"response.create", "response.created", "response.in_progress", "response.queued", "response.completed", "response.incomplete", "response.done", "response.failed", "response.error", "response.cancelled", "response.metadata", "response.interrupt", "response.interrupt.accepted", "response.interrupt.failed",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done", "response.output_text.annotation.added",
		"response.refusal.delta", "response.refusal.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.web_search_call.completed":
		return true
	}
	return false
}

// Chat refusal deltas are streamed as content by streamEvent, but their
// text is still a refusal diagnostic and must be protected.
func hasChatRefusalDelta(event []byte) bool {
	var data []byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimSpace(line[len("data:"):])...)
		}
	}
	var v struct {
		Choices []struct {
			Delta struct {
				Refusal json.RawMessage `json:"refusal"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &v) != nil {
		return false
	}
	for _, choice := range v.Choices {
		if len(choice.Delta.Refusal) > 0 && string(choice.Delta.Refusal) != "null" {
			return true
		}
	}
	return false
}

// A Chat chunk is protocol-shaped: ordinary answers live in choices[].delta
// while refusals and errors sit in named fields. Only those named fields are
// diagnostics. Scoping the scrub there keeps an ordinary answer intact when a
// short credential happens to overlap it ("world" -> "w[redacted]ld"), which
// rewriting the whole chunk did whenever one choice carried a refusal.
func chatRefusalPayload(body []byte, secrets []string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if dec.Decode(&root) != nil {
		return body, false
	}
	choices, ok := root["choices"].([]any)
	if !ok {
		return body, false
	}
	changed := false
	scrub := func(container map[string]any, key string) {
		value, exists := container[key]
		if !exists {
			return
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return
		}
		clean := hideFailureBody(raw, secrets)
		if bytes.Equal(raw, clean) {
			return
		}
		next := json.NewDecoder(bytes.NewReader(clean))
		next.UseNumber()
		var decoded any
		if next.Decode(&decoded) != nil {
			return
		}
		container[key] = decoded
		changed = true
	}
	scrub(root, "error")
	for _, item := range choices {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}
		scrub(choice, "error")
		scrub(choice, "refusal")
		if delta, ok := choice["delta"].(map[string]any); ok {
			scrub(delta, "error")
			scrub(delta, "refusal")
		}
		if message, ok := choice["message"].(map[string]any); ok {
			scrub(message, "error")
			scrub(message, "refusal")
		}
	}
	if !changed {
		return body, true
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	return out, true
}

// Keep the upstream's known credentials with its body so eventStream can
// protect errors even when the vendor labels an SSE reply as text/plain.
type upstreamCredentialBody struct {
	io.ReadCloser
	secrets []string
}

func protectErrorEvents(res *http.Response, reader io.Reader) io.Reader {
	body, ok := res.Body.(*upstreamCredentialBody)
	if !ok {
		return reader
	}
	res.ContentLength = -1
	res.Header.Del("Content-Length")
	return &upstreamErrorStream{body: body, reader: bufio.NewReader(reader), secrets: body.secrets}
}

// upstreamErrorStream reads complete SSE events, so a credential split across
// network reads cannot escape in an error event. Ordinary content is unchanged.
type upstreamErrorStream struct {
	body    io.ReadCloser
	reader  *bufio.Reader
	secrets []string
	pending []byte
	done    bool
	endErr  error
}

func (s *upstreamErrorStream) Read(p []byte) (int, error) {
	for len(s.pending) == 0 && !s.done {
		var event []byte
		comment := false
		for {
			part, err := s.reader.ReadSlice('\n')
			if len(event) == 0 {
				comment = bytes.HasPrefix(part, []byte(":"))
			}
			event = append(event, part...)
			if len(event) > 4<<20 {
				s.pending = []byte("event: error\ndata: {\"error\":{\"message\":\"Upstream event exceeded safe limit\"}}\n\n")
				s.done = true
				break
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			// A standalone SSE comment is a keepalive even without an empty
			// line. Buffer the complete line before masking, including lines
			// longer than the bufio reader's buffer.
			if comment && err == nil {
				s.pending = []byte(hideFailureSecrets(string(event), s.secrets))
				break
			}
			if err != nil {
				s.done = true
				if err != io.EOF {
					s.endErr = err
				}
				break
			}
			if bytes.Equal(part, []byte("\n")) || bytes.Equal(part, []byte("\r\n")) {
				break
			}
		}
		if len(s.pending) != 0 {
			break
		}
		if len(event) == 0 {
			break
		}
		// Comments attached to an ordinary event are not error payloads, but
		// are still upstream metadata and may contain a credential.
		parts := bytes.SplitAfter(event, []byte("\n"))
		for i, line := range parts {
			if bytes.HasPrefix(line, []byte(":")) {
				parts[i] = []byte(hideFailureSecrets(string(line), s.secrets))
			}
		}
		event = bytes.Join(parts, nil)
		kind, _, _ := streamEvent(event)
		namedError := false
		for _, line := range bytes.Split(event, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("event:")) {
				name := strings.TrimSpace(string(line[len("event:"):]))
				namedError = name == "error" || name == "response.failed"
			}
		}
		if namedError || kind == eventError || kind == eventRefusalError || kind == eventRefusal || hasChatRefusalDelta(event) {
			lines := bytes.SplitAfter(event, []byte("\n"))
			for i, line := range lines {
				if bytes.HasPrefix(line, []byte("data:")) {
					ending := []byte(nil)
					if bytes.HasSuffix(line, []byte("\r\n")) {
						ending = []byte("\r\n")
					} else if bytes.HasSuffix(line, []byte("\n")) {
						ending = []byte("\n")
					}
					payload := bytes.TrimSpace(line[len("data:"):])
					clean, chat := chatRefusalPayload(payload, s.secrets)
					if !chat {
						clean = hideFailureBody(payload, s.secrets)
					}
					lines[i] = append(append([]byte("data: "), clean...), ending...)
				} else if colon := bytes.IndexByte(line, ':'); colon >= 0 {
					if bytes.Equal(line[:colon], []byte("event")) {
						name := strings.TrimSpace(string(line[colon+1:]))
						if errorProtocolEvent(name) {
							continue
						}
					}
					// SSE metadata (including comments and IDs) can also carry
					// a credential in an upstream error.
					lines[i] = append(append([]byte(nil), line[:colon+1]...), []byte(hideFailureSecrets(string(line[colon+1:]), s.secrets))...)
				}
			}
			event = bytes.Join(lines, nil)
		}
		s.pending = event
	}
	if len(s.pending) == 0 {
		if s.endErr != nil {
			return 0, s.endErr
		}
		return 0, io.EOF
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *upstreamErrorStream) Close() error { return s.body.Close() }
