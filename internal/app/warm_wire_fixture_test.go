package app

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/threadindex"
)

// The browser test encodes its held-row marker and expected page independently
// of the server's warm-stream controller.
const warmRetryWait = 5 * time.Second
const warmJoinWait = 2 * time.Second
const warmJoining = 4
const warmBudget = 150

// warmPaneQuery is the page query a warm pane reads: exactly the Enhanced
// pane's, so the loop keys both to one read.
func warmPaneQuery() threadindex.TranscriptQuery {
	return threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, HiddenSpecified: true,
		HiddenCategories: threadindex.EnhancedHiddenCategories(), Limit: 40}
}

// warmHeldDigest names a run of rows as a browser holds them: every field
// the Enhanced view draws, for each row in order, through 64-bit FNV-1a. The
// browser computes the same over the rows it holds (warm_transcripts.js
// heldDigest: strings as they are, numbers in decimal, booleans as 1 or 0,
// an absent value as nothing), so any change to a held row made while the
// page was away is seen without sending the rows.
func warmHeldDigest(rows []threadindex.TranscriptItem) string {
	h := fnv.New64a()
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	for _, row := range rows {
		createdAt, originalBytes := "", ""
		if row.CreatedAt != nil {
			createdAt = *row.CreatedAt
		}
		if row.OriginalBytes != 0 {
			originalBytes = fmt.Sprint(row.OriginalBytes)
		}
		fmt.Fprintf(h, "%s\n%d\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%d\n%d\n%s\n%d\n%s\n",
			row.ID, row.ProjectedSeq, row.TurnID, row.NativeItemID, row.Type, row.Category, row.Role, row.Status,
			row.Text, createdAt, row.Phase, flag(row.IsFinalAnswer), flag(row.Sidechain), row.ContentRef,
			flag(row.ContentTruncated), originalBytes)
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// pageQueryKey names one page query: panes with the same key read the same
// page.
func pageQueryKey(query threadindex.TranscriptQuery) string {
	return fmt.Sprintf("%s\x00%s\x00%t\x00%d", query.View, strings.Join(query.HiddenCategories, ","), query.HiddenSpecified, query.Limit)
}
