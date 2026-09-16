package mcp

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one parsed Server-Sent Events event.
type sseEvent struct {
	Event string
	Data  string
	ID    string
}

// readSSEEvent reads the next event from r. It returns io.EOF (with any final
// event still populated) when the stream ends.
func readSSEEvent(r *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	var data []string
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if len(data) > 0 || ev.Event != "" {
					ev.Data = strings.Join(data, "\n")
					return ev, nil
				}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			default:
				field, value := splitSSEField(line)
				switch field {
				case "event":
					ev.Event = value
				case "data":
					data = append(data, value)
				case "id":
					ev.ID = value
				}
			}
		}
		if err != nil {
			if err == io.EOF && (len(data) > 0 || ev.Event != "") {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return ev, err
		}
	}
}

// splitSSEField splits an SSE "field: value" line. A missing colon yields the
// whole line as the field name with an empty value.
func splitSSEField(line string) (string, string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return line, ""
	}
	value := line[i+1:]
	value = strings.TrimPrefix(value, " ")
	return line[:i], value
}
