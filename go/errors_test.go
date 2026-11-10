package zipbox

import (
	"encoding/json"
	"net/http"
	"testing"
)

// AX-09: an error printed as JSON (the CLI's --json) says when to retry, under the
// envelope's own name, and only when the server said.
func TestErrorJSONCarriesRetryAfterMs(t *testing.T) {
	cases := []struct {
		retryAfter string
		want       any
	}{
		{"2", float64(2000)},
		{"0", float64(0)},
		{"", nil},
	}
	for _, tc := range cases {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
		if tc.retryAfter != "" {
			resp.Header.Set("Retry-After", tc.retryAfter)
		}
		zerr := errorFromResponse(resp, []byte(`{"error":{"code":"rate_limited","message":"slow","requestId":"r","retryable":true}}`))
		data, err := json.Marshal(zerr)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		if out["retryAfterMs"] != tc.want || out["retryable"] != true || out["requestId"] != "r" || out["code"] != "rate_limited" {
			t.Fatalf("Retry-After %q: %s", tc.retryAfter, data)
		}
	}
}
