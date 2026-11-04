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
				if k == "type" && errorProtocolType(item) {
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
	case "error", "message", "type", "code", "param", "status", "response", "id", "object", "choices", "delta", "index", "finish_reason", "stop_reason", "stop_details", "content_block", "content", "text", "refusal", "usage", "request_id":
		return true
	}
	return false
}

func errorProtocolType(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	switch s {
	case "error", "response.failed", "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "rate_limit_error", "api_error", "overloaded_error", "billing_error", "server_error":
		return true
	}
	return false
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
		if namedError || kind == eventError || kind == eventRefusalError || kind == eventRefusal {
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
					lines[i] = append(append([]byte("data: "), hideFailureBody(payload, s.secrets)...), ending...)
				} else if colon := bytes.IndexByte(line, ':'); colon >= 0 {
					if bytes.Equal(line[:colon], []byte("event")) {
						name := strings.TrimSpace(string(line[colon+1:]))
						if name == "error" || name == "response.failed" {
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
