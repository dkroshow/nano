package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/threadindex"
)

const (
	// warmSetMax is how many threads one page may ask for; the first are
	// kept. A sidebar renders fewer rows than this.
	warmSetMax = 100
	// WarmJoining is how many threads join one stream at once: the next
	// joins when one of them has sent its first message, or has waited
	// WarmJoinWait without one (a projection still indexing sends nothing).
	WarmJoining  = 4
	WarmJoinWait = 2 * time.Second
	// warmStreamBacklog is a warm stream's queue limit. The threads joining
	// at once, each at the largest page the catalog keeps inline (40 rows of
	// 8 KiB), fit in half of it, so a connect burst never ends the stream.
	warmStreamBacklog = 4 << 20
	// warmPageRows is the page a warm pane reads: the same page a pane on
	// screen reads (TRANSCRIPT_LIMIT in web/src/transcript_query.js), so the
	// two share one read.
	warmPageRows = 40
)

// warmPaneQuery is the page query a warm pane reads: exactly the Enhanced
// pane's, so the loop keys both to one read.
func warmPaneQuery() threadindex.TranscriptQuery {
	return threadindex.TranscriptQuery{View: threadindex.DefaultThreadTranscriptView, HiddenSpecified: true,
		HiddenCategories: threadindex.EnhancedHiddenCategories(), Limit: warmPageRows}
}

// WarmMarker is what a page holds of one thread: a projection key, the
// sequence range of its rows, and a digest of those rows (warmHeldDigest).
type WarmMarker struct {
	Key         string
	First, Last int64
	Digest      string
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

// WarmRequest is what a page asks for on its stream URL.
type WarmRequest struct {
	Set     []string
	Fill    WarmFill
	Markers map[string]WarmMarker
}

var errWarmMarkers = errors.New("markers must be a JSON object of thread ref to {key, first, last, digest}")

// WarmRetryWait is how long a thread that failed (a read error, a page the
// stream could not take) stays out before the stream tries it again: long
// enough that a thread failing every time costs one read in that time.
const WarmRetryWait = 5 * time.Second

// ParseWarmRequest validates a browser warm-stream request.
func ParseWarmRequest(r *http.Request) (WarmRequest, error) {
	q := r.URL.Query()
	raw := q.Get("set")
	if raw == "" {
		raw = q.Get("hints") // a page from before the set, until it reloads
	}
	var set []string
	for _, ref := range strings.Split(raw, ",") {
		if ref = strings.TrimSpace(ref); ref != "" && !slices.Contains(set, ref) {
			set = append(set, ref)
		}
	}
	if len(set) > warmSetMax {
		set = set[:warmSetMax]
	}
	fill, err := parseWarmFill(q.Get("lines"), q.Get("cols"))
	if err != nil {
		return WarmRequest{}, err
	}
	markers := make(map[string]WarmMarker)
	if raw := q.Get("markers"); raw != "" {
		var wire map[string]struct {
			Key    string `json:"key"`
			First  int64  `json:"first"`
			Last   int64  `json:"last"`
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			return WarmRequest{}, errWarmMarkers
		}
		for ref, m := range wire {
			if m.Key != "" && m.First > 0 && m.Last >= m.First && m.Digest != "" {
				markers[ref] = WarmMarker{Key: m.Key, First: m.First, Last: m.Last, Digest: m.Digest}
			}
		}
	}
	return WarmRequest{Set: set, Fill: fill, Markers: markers}, nil
}

// WarmStream is one page's warm stream and its counters.
type WarmStream struct {
	queue    *liveTextQueue
	openedAt time.Time
	fill     WarmFill
	refs     atomic.Int64
	reads    atomic.Int64
	bytes    atomic.Int64
	// Tails sent: the connect burst, then one per join, rebuild or gap.
	tails     atomic.Int64
	tailBytes atomic.Int64
}

// WarmStreamReport is one warm stream on GET /api/v1/monitor.
type WarmStreamReport struct {
	OpenedAt  time.Time `json:"openedAt"`
	Lines     int       `json:"lines"`
	Cols      int       `json:"cols"`
	Refs      int64     `json:"refs"`
	Reads     int64     `json:"reads"`
	Bytes     int64     `json:"bytes"`
	Tails     int64     `json:"tails"`
	TailBytes int64     `json:"tailBytes"`
}

// NewWarmStream creates the bounded shared queue for a browser warm stream.
func NewWarmStream(fill WarmFill) *WarmStream {
	return &WarmStream{queue: newLiveTextQueueWithBacklog(warmStreamBacklog), openedAt: time.Now().UTC(), fill: fill}
}

// Report snapshots this stream's counters without reading another owner.
func (stream *WarmStream) Report() WarmStreamReport {
	return WarmStreamReport{OpenedAt: stream.openedAt, Lines: stream.fill.lines, Cols: stream.fill.cols, Refs: stream.refs.Load(), Reads: stream.reads.Load(), Bytes: stream.bytes.Load(), Tails: stream.tails.Load(), TailBytes: stream.tailBytes.Load()}
}
