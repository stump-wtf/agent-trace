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
