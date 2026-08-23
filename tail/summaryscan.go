package tail

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
)

// Bounded Session Summarization
//
// Summarizing a session needs its head (identity, start time, title, sidechain
// stamp) and the timestamp on its final line. Recovering those by reading every
// line made one discovery pass cost the size of the whole session corpus — on a
// real archive, 906 MB and 11.8s per pass against a 2s poll interval, which no
// amount of polling restraint can absorb.
//
// scanJSONLSummary reads a bounded head and, only when a file exceeds that
// budget, a bounded tail. Lines are delivered in file order with the middle
// elided, so the first-wins and last-wins field semantics callers rely on are
// preserved. A file that fits inside the head budget is consumed whole and its
// summary is byte-identical to a full scan.
//
// @joestump-agent 08/23/2026 - Extracted from the adapters' Summarize methods
// to fix the CPU burn in #79.
//
// @joestump-agent 08/23/2026 - Made the head's byte budget a hard bound rather
// than a post-hoc check, so one newline-free line cannot be read whole (#84).
//
// @joestump-agent 08/23/2026 - Report whether the final line was actually
// delivered, so a caller cannot mistake a head timestamp for the file's last
// activity (#85).

// summaryBudget bounds how much of a session file a summary scan reads.
type summaryBudget struct {
	// maxLines and maxBytes bound the head scan. Both apply; whichever is
	// reached first ends the head. The byte bound exists so a file made of a
	// few enormous lines cannot defeat the line bound.
	//
	// maxBytes is a hard bound on what the head reads, not just on what it
	// has already read: the head is fed through an io.LimitReader, so a
	// single line longer than the budget is cut off rather than grown to
	// whatever length the file happens to contain. The truncated fragment is
	// discarded rather than delivered — it is not valid JSON, and the head
	// ends at the last real line boundary either way.
	maxLines int
	maxBytes int64
	// tailBytes is how much of the end of an oversized file is read back to
	// recover the final timestamp.
	tailBytes int64
}

// defaultSummaryBudget returns the budget used in production. The values are
// measured, not guessed, against a real 406-session / 906 MB Claude Code
// corpus. Both bounds are load-bearing and they guard different failure modes.
//
// A transcript's metadata is concentrated in its first couple of dozen lines,
// but those lines are enormous — the system prompt and injected context run to
// tens of kilobytes each. Measured position of the first line carrying
// message.model, which is the deepest field the head must reach:
//
//	by line    p50=12   p90=16   p99=21   max=23
//	by byte    p50=73K  p90=89K  p95=255K max=1114K
//
// So the line bound is the one that describes the data (64 clears max=23 by
// almost 3x) and the byte bound is a backstop against a pathological file,
// sized past the 1114K worst case. Capping bytes tighter than that is the
// tempting mistake: 64 KiB looks fine on cost and silently truncates the head
// before the model line on 17 of 344 sessions, after which Model is picked up
// from the tail and reports the model the session *ended* on rather than the
// one it started with.
//
// Cost of one full discovery pass, and fidelity against a full scan:
//
//	512 lines / 64 KiB   0.7s   17 sessions wrong
//	 64 lines / 256 KiB  1.2s   exact
//	 64 lines / 2 MiB    1.4s   exact      <- chosen
//	512 lines / 2 MiB    5.0s   exact
//
// Title is the one field this budget can miss: an ai-title line sits at p50=9,
// p90=13, max=453, so a session that titles itself unusually late falls back to
// the file name — the same fallback an untitled session already gets, and
// cosmetic either way. Paying 5.0s to chase that one outlier is not worth it.
//
// tailBytes is sized for headroom rather than to the measurement: 16 KiB
// already recovered every closing timestamp in the corpus, but a session whose
// final line is one large tool result would lose EndedAt, so this keeps room to
// spare. Headroom is not a guarantee — no fixed window is, since a tool result
// has no upper bound — so the scan reports when the window held no whole line
// and Summarize dates the session by its mtime instead (#85). All of this is
// cold-scan cost only — summaryCache serves unchanged files without reading
// them at all.
func defaultSummaryBudget() summaryBudget {
	return summaryBudget{
		maxLines:  64,
		maxBytes:  2 << 20,
		tailBytes: 64 << 10,
	}
}

// summaryScan reports what a bounded scan actually managed to read, which is
// not the same question as whether it errored. Both fields are ways of asking
// "how much should the caller trust the fields it just collected".
type summaryScan struct {
	// Complete reports that the head consumed the whole file, so the summary
	// is byte-identical to what a full scan would have produced.
	Complete bool
	// SawFinalLine reports that the file's last line reached visit — always
	// true for a complete read, and for an elided one only when the tail
	// window held a whole line.
	//
	// When it is false, every last-wins field a caller collected still holds
	// whatever the *head* last set, which is a timestamp from the top of the
	// file rather than the bottom. A caller that treats such a value as the
	// session's last activity gets an answer that is not merely imprecise but
	// wrong by the length of the session — see ClaudeCodeAdapter.Summarize,
	// which substitutes the file's mtime rather than report it (#85).
	SawFinalLine bool
}

// scanJSONLSummary feeds visit the head of a JSONL session file and, when the
// file is larger than the head budget, the trailing whole lines from its tail.
func scanJSONLSummary(f *os.File, visit func([]byte)) (summaryScan, error) {
	return scanJSONLSummaryWith(f, defaultSummaryBudget(), visit)
}

// scanJSONLSummaryWith is scanJSONLSummary with an explicit budget, so tests can
// exercise the oversized path without building multi-megabyte fixtures.
func scanJSONLSummaryWith(f *os.File, b summaryBudget, visit func([]byte)) (summaryScan, error) {
	headEnd, complete, err := scanSummaryHead(f, b, visit)
	if err != nil || complete {
		return summaryScan{Complete: complete, SawFinalLine: complete}, err
	}
	sawFinal, err := scanSummaryTail(f, b, headEnd, visit)
	return summaryScan{SawFinalLine: sawFinal}, err
}

// scanSummaryHead reads whole lines until a budget is reached or the file ends.
// It returns the byte offset just past the last line it consumed — always a line
// boundary, which is what lets the tail scan resume without truncating a line —
// and whether it reached EOF.
//
// The reads run through an io.LimitReader so maxBytes bounds the memory this
// can hold, not merely the point at which it stops. bufio.Reader.ReadBytes
// grows its result until it finds the delimiter or the reader errors, so
// without the limiter one line with no newline in it — a corrupt or foreign
// .jsonl, which this scan is pointed at whatever the trajectory directories
// contain — is pulled entirely into memory before the budget is consulted.
func scanSummaryHead(f *os.File, b summaryBudget, visit func([]byte)) (int64, bool, error) {
	// The limiter is given one byte of headroom past the budget so a genuine
	// end-of-file that lands exactly on maxBytes is distinguishable from a line
	// the limiter cut off. Without it the two are identical — err is io.EOF and
	// the bytes in hand end at the budget either way — and the real final line
	// gets dropped as if it were a fragment.
	reader := bufio.NewReaderSize(io.LimitReader(f, b.maxBytes+1), 64*1024)
	var consumed int64
	for lines := 0; ; lines++ {
		if lines >= b.maxLines || consumed >= b.maxBytes {
			return consumed, false, nil
		}
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && consumed+int64(len(line)) > b.maxBytes {
			// The limiter cut this line off, so what is in hand is a leading
			// fragment rather than a record — the same thing scanSummaryTail
			// drops when its seek lands mid-line, and for the same reason:
			// handing it to visit is indistinguishable from a corrupt record.
			// consumed stays where it is so the returned offset remains a
			// true line boundary and the tail scan can resume from it.
			//
			// A file whose real end coincides exactly with the budget does
			// NOT take this path: the headroom byte above proves the file
			// ended, so its final line is delivered whole and the head
			// reports complete, exactly as a full scan would.
			return consumed, false, nil
		}
		consumed += int64(len(line))
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
			visit(trimmed)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return consumed, true, nil
			}
			return consumed, false, err
		}
	}
}

// scanSummaryTail delivers the whole lines at the end of an oversized file,
// picking up no earlier than headEnd so no line is delivered twice. It reports
// whether it delivered anything at all.
//
// Delivering nothing is not an error and not rare: the window is a fixed size
// and a session's final line is a tool result, which can be larger than it. It
// is reported rather than swallowed because the caller cannot otherwise tell
// that outcome from "read the tail, found nothing newer" — and the fields it
// collected differ completely between the two.
func scanSummaryTail(f *os.File, b summaryBudget, headEnd int64, visit func([]byte)) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := info.Size()

	start, dropPartial := size-b.tailBytes, true
	if start <= headEnd {
		// The tail window reaches back into what the head already covered.
		// Resume exactly where the head stopped: that offset is a line
		// boundary, so there is no partial line to discard.
		start, dropPartial = headEnd, false
	}
	if start >= size {
		return false, nil
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return false, err
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	if dropPartial {
		// The seek landed mid-line. That leading fragment is not valid JSON,
		// and handing it to visit would be indistinguishable from a corrupt
		// record, so drop it. No newline anywhere in the window means the
		// window holds no whole line at all.
		if _, err := reader.ReadBytes('\n'); err != nil {
			return false, nil
		}
	}
	delivered := false
	err = ReadJSONLines(reader, func(data []byte) {
		delivered = true
		visit(data)
	})
	return delivered, err
}
