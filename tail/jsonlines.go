package tail

import (
	"bufio"
	"bytes"
	"io"
	"os"
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

// jsonlFirstLine returns the first newline-terminated record of the file, or
// nil when there is none. It reads exactly one record rather than streaming
// the file: the incremental adapters use it to recover session-head metadata
// (cwd, id) that sits before their byte-offset watermarks, and scanning to
// EOF for a first line would reintroduce the very cost ParseSince exists to
// avoid.
func jsonlFirstLine(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	// A read error with partial data still yields a first line to try;
	// callers that fail to unmarshal it treat the metadata as absent.
	line, _ := bufio.NewReaderSize(f, 64*1024).ReadBytes('\n')
	if len(line) == 0 {
		return nil
	}
	return bytes.TrimRight(line, "\r\n")
}

// jsonlLastLineBefore returns the complete record that ends immediately
// before the given byte offset — the record a watermark at that offset last
// consumed — or nil when there is none. offset is assumed to be a record
// boundary of the kind readCompleteJSONLines produces.
//
// The read is a bounded backwards scan, not a stream from the start: this
// runs on every poll in the Pi adapter's continuation check, and the record
// it wants is at the tail.
func jsonlLastLineBefore(path string, offset int64) []byte {
	if offset <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	window := int64(8192)
	for {
		start := offset - window
		if start < 0 {
			start = 0
		}
		buf := make([]byte, offset-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil
		}
		if len(buf) == 0 {
			return nil
		}
		// The byte at offset-1 is the terminator of the record we want; the
		// record itself starts just past the previous terminator.
		head := buf[:len(buf)-1]
		if idx := bytes.LastIndexByte(head, '\n'); idx >= 0 {
			return bytes.TrimRight(head[idx+1:], "\r\n")
		}
		if start == 0 {
			return bytes.TrimRight(head, "\r\n")
		}
		window *= 2
	}
}
