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
	return scanJSONLines(r, func(line []byte) error {
		visit(line)
		return nil
	})
}

// scanJSONLines is ReadJSONLines with a way out: it stops at, and returns,
// the first error visit returns. Each line is handed over as soon as its
// terminator is read, so on a pipe a record is visited while the writer is
// still running.
func scanJSONLines(r io.Reader, visit func([]byte) error) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 {
				if verr := visit(line); verr != nil {
					return verr
				}
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
	lines := jsonlLeadingLines(path, 1)
	if len(lines) == 0 {
		return nil
	}
	return lines[0]
}

// jsonlLeadingLines returns up to the first n records of the file, each as
// jsonlFirstLine returns one, stopping early at end of file or at an empty
// read. Like jsonlFirstLine it reads those records and no further: an OMP
// session keeps its header on the second line, behind a title slot.
func jsonlLeadingLines(path string, n int) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 64*1024)
	var lines [][]byte
	for len(lines) < n {
		// A read error with partial data still yields a line to try;
		// callers that fail to unmarshal it treat the metadata as absent.
		line, err := r.ReadBytes('\n')
		if len(line) == 0 {
			break
		}
		lines = append(lines, bytes.TrimRight(line, "\r\n"))
		if err != nil {
			break
		}
	}
	return lines
}

// jsonlLastLineBefore returns the complete record that ends immediately
// before the given byte offset — the record a watermark at that offset last
// consumed — or nil when there is none. offset is assumed to be a record
// boundary of the kind readCompleteJSONLines produces.
//
// The read is a bounded backwards scan, not a stream from the start: this
// runs on every poll in the Pi adapter's continuation check, and the record
// it wants is at the tail.
//
// The window doubles until it finds a record boundary and then stops at
// jsonlLastLineCap. Without the cap the growth is the file's own size, so one
// enormous record — a big tool result, the ordinary shape of a transcript's
// last line — is pulled whole into memory on every poll. Giving up returns
// nil, which the caller reads as "cannot confirm this is a linear append" and
// answers with a full parse: slower on that poll, never wrong.
func jsonlLastLineBefore(path string, offset int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	line, _, ok := jsonlRecordBefore(f, offset)
	if !ok {
		return nil
	}
	return line
}

// jsonlRecordBefore returns the complete record that ends immediately before
// offset in f, and the offset at which that record starts. ok is false when
// there is no such record or it cannot be read within jsonlLastLineCap. See
// jsonlLastLineBefore for the windowing.
func jsonlRecordBefore(f *os.File, offset int64) (line []byte, start int64, ok bool) {
	if offset <= 0 {
		return nil, 0, false
	}
	window := int64(8192)
	for {
		if window > jsonlLastLineCap {
			return nil, 0, false
		}
		from := max(offset-window, 0)
		buf := make([]byte, offset-from)
		if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
			return nil, 0, false
		}
		if len(buf) == 0 {
			return nil, 0, false
		}
		// The byte at offset-1 is the terminator of the record we want; the
		// record itself starts just past the previous terminator.
		head := buf[:len(buf)-1]
		if idx := bytes.LastIndexByte(head, '\n'); idx >= 0 {
			return bytes.TrimRight(head[idx+1:], "\r\n"), from + int64(idx) + 1, true
		}
		if from == 0 {
			return bytes.TrimRight(head, "\r\n"), 0, true
		}
		window *= 2
	}
}

// jsonlLastMatchBefore walks the complete records that end at or before
// offset, newest first, and returns the first one match accepts, or nil when
// none does.
//
// The incremental readers use it to recover state a record before their
// watermark left behind — the API response a Claude Code usage report
// belongs to, the model a Codex turn runs on — so that a read resuming
// mid-session reports exactly what a full read would. The record wanted is
// normally a few records back. The walk gives up after jsonlLastMatchCap
// bytes rather than read a long session backwards on every poll; a caller
// treats nil as "nothing recorded".
func jsonlLastMatchBefore(path string, offset int64, match func([]byte) bool) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	for end := offset; end > 0 && offset-end <= jsonlLastMatchCap; {
		line, start, ok := jsonlRecordBefore(f, end)
		if !ok {
			return nil
		}
		if len(line) > 0 && match(line) {
			return line
		}
		end = start
	}
	return nil
}

// jsonlLastMatchCap bounds how far jsonlLastMatchBefore walks back.
const jsonlLastMatchCap = 16 << 20

// jsonlLastLineCap bounds jsonlLastLineBefore's backwards scan. It is sized
// well past a normal record and well below a pathological one: the largest
// line in the measured Claude Code corpus is ~1.1 MiB (see
// defaultSummaryBudget), so a real record is found long before this, and a
// record that is not is one no per-poll read should be growing to fit.
const jsonlLastLineCap = 4 << 20
