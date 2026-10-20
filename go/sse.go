package zipbox

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one server-sent event.
type sseEvent struct {
	id    string
	event string
	data  string
}

// sseReader parses a text/event-stream body (WHATWG rules: `field: value`
// lines, multi-line data joined with "\n", a blank line dispatches, `:` lines
// are comments, CRLF and LF both end a line).
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// next returns the next event, or io.EOF when the stream ends cleanly between
// events. A stream that ends mid-event returns io.ErrUnexpectedEOF.
func (s *sseReader) next() (sseEvent, error) {
	var ev sseEvent
	var data []string
	seen := false
	for {
		line, err := s.r.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			if err == io.EOF {
				if seen {
					return sseEvent{}, io.ErrUnexpectedEOF
				}
				return sseEvent{}, io.EOF
			}
			return sseEvent{}, err
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if !seen {
				continue
			}
			ev.data = strings.Join(data, "\n")
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		seen = true
		switch field {
		case "id":
			ev.id = value
		case "event":
			ev.event = value
		case "data":
			data = append(data, value)
		}
		if err == io.EOF {
			return sseEvent{}, io.ErrUnexpectedEOF
		}
	}
}
