package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// csvHeader is the export contract. Columns are appended, never reordered or
// removed: an auditor's spreadsheet template outlives any release of this tool.
var csvHeader = []string{
	"seq",
	"recorded_at",
	"repo",
	"pr",
	"commit",
	"address",
	"resource_type",
	"action",
	"author",
	"approvers",
	"protected",
	"protected_by",
	"acknowledged",
	"entry_hash",
}

// WriteCSV renders entries as CSV, one row per resource change.
func WriteCSV(w io.Writer, entries []Entry) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, e := range entries {
		row := []string{
			strconv.Itoa(e.Seq),
			e.RecordedAt.UTC().Format(time.RFC3339),
			e.Repo,
			strconv.Itoa(e.PR),
			e.Commit,
			e.Address,
			e.Type,
			e.Action,
			e.Author,
			strings.Join(e.Approvers, " "),
			strconv.FormatBool(e.Protected),
			e.ProtectedBy,
			strconv.FormatBool(e.Acknowledged),
			e.Hash,
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("write csv row %d: %w", e.Seq, err)
		}
	}
	cw.Flush()
	return cw.Error()
}
