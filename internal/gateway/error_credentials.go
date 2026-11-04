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
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		for _, value := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret), string(encoded[1 : len(encoded)-1])} {
			if value != "" {
				s = strings.ReplaceAll(s, value, "[redacted]")
			}
		}
	}
	return s
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
		case []any:
			for i := range v {
				v[i] = scrub(v[i])
			}
		case map[string]any:
			out := make(map[string]any, len(v))
			for k, item := range v {
				name := k
				for _, secret := range secrets {
					if secret != "" && (k == secret || len(secret) >= 8 && strings.Contains(k, secret)) {
						name = hideFailureSecrets(name, []string{secret})
					}
				}
				changed = changed || name != k
				if _, exists := out[name]; exists {
					return "[redacted]"
				}
				out[name] = scrub(item)
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
		for {
			part, err := s.reader.ReadSlice('\n')
			// A standalone SSE comment is a keepalive even without an empty
			// line (another Magpie sends these while a request is queued).
			if len(event) == 0 && err == nil && bytes.HasPrefix(part, []byte(":")) {
				s.pending = append([]byte(nil), part...)
				break
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
		kind, _, _ := streamEvent(event)
		if kind == eventError {
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
