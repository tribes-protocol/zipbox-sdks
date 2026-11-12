package zipbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// claimStreamMediaType is what Create asks for: its answer streamed once the box is
// placed.
const claimStreamMediaType = "application/x-ndjson"

type claimStreamLine struct {
	Type    string            `json:"type"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

// readClaimStream reads a create's answer (conformance README, streamed create). A server
// that streamed it sends {"type":"placed","baseUrl"} the moment the box is placed, then
// {"type":"answer","status","headers","body"}: the answer it would have given
// unstreamed, returned as that response so it is read exactly like one. The placed line
// is informational and never dialled: its origin is handed out before its DNS record is
// known to be live. Any other response is returned as it is. A stream that ends before
// its answer is an error, retried like a dropped connection: Create sends the same
// Idempotency-Key and gets the same box.
func readClaimStream(resp *http.Response) (*http.Response, error) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.EqualFold(mediaType, claimStreamMediaType) {
		return resp, nil
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
			var line claimStreamLine
			if err := json.Unmarshal(trimmed, &line); err != nil {
				return nil, err
			}
			// An answer names its status; a line without one is not the answer (the
			// other SDKs keep reading too), so a cut-short object ends in the retryable
			// "ended before its answer" below, never a status-0 refusal.
			if line.Type == "answer" && line.Status != 0 {
				// Read to its end (the server closes right after): the connection goes
				// back to the pool.
				_, _ = io.Copy(io.Discard, reader)
				return claimAnswer(resp, line), nil
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, errors.New("the create stream ended before its answer")
			}
			return nil, readErr
		}
	}
}

func claimAnswer(resp *http.Response, line claimStreamLine) *http.Response {
	header := http.Header{}
	for name, value := range line.Headers {
		header.Set(name, value)
	}
	header.Set("Content-Type", "application/json")
	body := []byte(line.Body)
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		body = nil
	}
	return &http.Response{
		Status:        http.StatusText(line.Status),
		StatusCode:    line.Status,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       resp.Request,
	}
}
