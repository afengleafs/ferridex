// Package util holds small, dependency-free helpers shared across ferridex's
// provider and server packages: SSE stream parsing, JSON/map accessors, and
// JWT claim decoding.
package util

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// SSEEvent is one parsed Server-Sent Event.
type SSEEvent struct {
	EventType string
	Data      string
}

// IterSSEEvents parses an SSE stream: events end on a blank line; multiple
// data: lines are joined with "\n".
func IterSSEEvents(r io.Reader, yield func(SSEEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		eventType string
		dataLines []string
		hasData   bool
	)
	dispatch := func() error {
		if !hasData {
			eventType = ""
			return nil
		}
		ev := SSEEvent{EventType: eventType, Data: strings.Join(dataLines, "\n")}
		eventType = ""
		dataLines = dataLines[:0]
		hasData = false
		return yield(ev)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field = line
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
			hasData = true
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading SSE stream: %w", err)
	}
	return dispatch()
}
