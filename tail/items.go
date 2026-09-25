package tail

import (
	"context"
	"fmt"

	"github.com/stump-wtf/agent-trace/classify"
)

// Items is everything one read of a session yields, in one seq space: the
// classified tool events, the timeline marks, and the usage reports (#105).
type Items struct {
	Events []classify.Event
	Marks  []classify.Mark
	Usage  []classify.Usage
}

// ItemParser is an optional interface for adapters that read usage reports
// (classify.Usage) as well as events and marks. ParseItems and
// ParseItemsSince are Parse and ParseSince with the usage kept: the same
// read, the same seq numbers, the same watermark. Parse and ParseSince are
// exactly these with Items.Usage dropped, so a consumer that does not want
// usage loses nothing by staying on them.
//
// Usage items follow the rules marks do. A Usage's Seq is the seq of the
// next event, like a Mark's, so it precedes that event and consumes no seq
// of its own. ParseItemsSince withholds usage past its watermark exactly as
// it withholds events and marks, so repeated incremental reads deliver each
// usage item once. A session whose transcript records no usage yields no
// Usage items — never zero-valued ones.
//
// It is optional so that adding usage broke no existing Adapter or caller;
// use the package-level ParseItems and ParseItemsSince to read any adapter,
// with usage where the adapter supplies it. The Claude Code, Codex and Crush
// adapters implement it; see the README for which fields each fills.
type ItemParser interface {
	ParseItems(ctx context.Context, path string) (Items, SessionMeta, error)
	ParseItemsSince(ctx context.Context, path string, watermark int64, startSeq int) (Items, SessionMeta, int64, error)
}

// ParseItems reads a whole session through a. When a implements ItemParser
// the result carries its usage reports; otherwise it is a's Parse with no
// usage.
func ParseItems(ctx context.Context, a Adapter, path string) (Items, SessionMeta, error) {
	if ip, ok := a.(ItemParser); ok {
		return ip.ParseItems(ctx, path)
	}
	events, marks, meta, err := a.Parse(ctx, path)
	return Items{Events: events, Marks: marks}, meta, err
}

// ParseItemsSince reads a session incrementally through a, as
// IncrementalParser.ParseSince does. When a implements ItemParser the result
// carries its usage reports; otherwise it is a's ParseSince with no usage. It
// returns an error when a supports neither.
func ParseItemsSince(ctx context.Context, a Adapter, path string, watermark int64, startSeq int) (Items, SessionMeta, int64, error) {
	if ip, ok := a.(ItemParser); ok {
		return ip.ParseItemsSince(ctx, path, watermark, startSeq)
	}
	inc, ok := a.(IncrementalParser)
	if !ok {
		return Items{}, SessionMeta{}, 0, fmt.Errorf("%s adapter does not parse incrementally", a.Harness())
	}
	events, marks, meta, wm, err := inc.ParseSince(ctx, path, watermark, startSeq)
	return Items{Events: events, Marks: marks}, meta, wm, err
}
