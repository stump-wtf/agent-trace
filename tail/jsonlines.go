package tail

import (
	"bufio"
	"bytes"
	"io"
)

// ReadJSONLines streams JSON lines from r, calling visit for each non-empty
// line. Handles both \n and \r\n line endings. Returns nil on EOF.
func ReadJSONLines(r io.Reader, visit func([]byte)) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 {
				visit(line)
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// readCompleteJSONLines streams only newline-terminated JSON lines from r,
// calling visit with the line and the absolute byte offset just past its
// terminator (base + bytes consumed so far).
//
// The trailing line is deliberately skipped when it has no terminator. A live
// harness appends to its session log with ordinary buffered writes, so a poll
// can land in the middle of a record; ReadJSONLines visits that fragment, and
// an incremental reader that then remembered the file size as its watermark
// would resume past the fragment and lose the record for good. Stopping at the
// last complete line means the watermark is always a record boundary and the
// partial line is simply re-read, whole, on the next poll.
func readCompleteJSONLines(r io.Reader, base int64, visit func(line []byte, end int64)) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	offset := base
	for {
		raw, err := reader.ReadBytes('\n')
		if err == nil {
			offset += int64(len(raw))
			line := bytes.TrimRight(raw, "\r\n")
			if len(line) > 0 {
				visit(line, offset)
			}
			continue
		}
		if err == io.EOF {
			return nil // trailing bytes without '\n' are an incomplete record
		}
		return err
	}
}
