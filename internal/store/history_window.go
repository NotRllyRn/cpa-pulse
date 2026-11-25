package store

import (
	"context"
	"sort"
	"time"
)

// HistoryWindow holds records within the range and the last preceding sample
// of each series. KnownReset marks series with a nonzero reset in retained history.
type HistoryWindow struct {
	Records    []Record
	Before     map[string]Record
	KnownReset map[string]bool
}

// QueryWindow scans retained history once, using O(series + records in range)
// memory. The latest preceding sample wins, with the last appended record
// breaking ties. Only records within the range are stably sorted.
func (h *History) QueryWindow(ctx context.Context, from, to time.Time) (HistoryWindow, error) {
	out := HistoryWindow{Before: map[string]Record{}, KnownReset: map[string]bool{}}
	err := h.Scan(ctx, time.Time{}, to, func(record Record) error {
		if record.Kind != "e" {
			key := record.Group + "|" + record.Window
			if record.Kind == "s" && record.Reset != 0 {
				out.KnownReset[key] = true
			}
			if record.Time < from.Unix() {
				previous, exists := out.Before[key]
				if !exists || record.Time >= previous.Time {
					out.Before[key] = record
				}
				return nil
			}
		} else if record.Time < from.Unix() {
			return nil
		}
		out.Records = append(out.Records, record)
		return nil
	})
	if err != nil {
		return HistoryWindow{}, err
	}
	sort.SliceStable(out.Records, func(i, j int) bool { return out.Records[i].Time < out.Records[j].Time })
	if err := ctx.Err(); err != nil {
		return HistoryWindow{}, err
	}
	return out, nil
}
