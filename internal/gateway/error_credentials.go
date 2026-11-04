package gateway

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

// hideFailureSecrets removes credentials known to this attempt even when a
// vendor returns one in an error. Do not rely on the credential's shape.
func hideFailureSecrets(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for _, value := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
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
			for k := range v {
				v[k] = scrub(v[k])
			}
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
