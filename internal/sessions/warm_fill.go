package sessions

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/threadindex"
)

// The fill rule: a warm pane sends the newest rows whose estimated height
// fills the browser's window, measured in lines and characters of the
// message font, which the page reports on its warm stream. The estimate
// leans low where it can (chrome below the measured minimum, markdown
// markers counted as width), so its usual error sends one row more rather
// than one too few: too few leaves a gap that jumps when the pane's own tail
// lands; too many costs bytes. It is an estimate of a markdown renderer, not
// the renderer: a layout it does not model can still leave a gap.
const (
	// A 2560x1440 window at the smallest text size, for a page that reports
	// nothing: it costs a little less than the old fixed 20 rows.
	warmFillLinesDefault = 98
	warmFillColsDefault  = 437
	warmFillLinesMin     = 5
	warmFillLinesMax     = 1000
	warmFillColsMin      = 20
	warmFillColsMax      = 2000
	// A fenced code block shows at most 360px and scrolls inside.
	warmFillCodeBlockLines = 18
	// Per-message chrome (label, padding, timestamp), below the measured
	// minimums of 1.3 and 2.9 lines.
	warmFillUserChrome  = 1.0
	warmFillAgentChrome = 2.5
)

var errWarmFillInvalid = errors.New("lines and cols must be integers within range")

// WarmFill is the bounded browser geometry used to size a saved transcript tail.
type WarmFill struct{ lines, cols int }

// ParseWarmFill reads a page's window from its stream URL: both absent is the
// default; anything else must be two in-range integers.
func ParseWarmFill(linesRaw, colsRaw string) (WarmFill, error) {
	if linesRaw == "" && colsRaw == "" {
		return WarmFill{lines: warmFillLinesDefault, cols: warmFillColsDefault}, nil
	}
	lines, err := strconv.Atoi(linesRaw)
	if err != nil || lines < warmFillLinesMin || lines > warmFillLinesMax {
		return WarmFill{}, errWarmFillInvalid
	}
	cols, err := strconv.Atoi(colsRaw)
	if err != nil || cols < warmFillColsMin || cols > warmFillColsMax {
		return WarmFill{}, errWarmFillInvalid
	}
	return WarmFill{lines: lines, cols: cols}, nil
}

// count is how many of the newest rows of a page (oldest first, as read)
// fill the window: at least one, every row when they all fit.
func (f WarmFill) count(items []threadindex.TranscriptItem) int {
	height := 0.0
	for i := len(items) - 1; i >= 0; i-- {
		height += estimateRowLines(items[i], f.cols)
		if height >= float64(f.lines) {
			return len(items) - i
		}
	}
	return len(items)
}

var (
	markdownLinkTarget = regexp.MustCompile(`\]\([^)]*\)`)
	markdownBlockStart = regexp.MustCompile(`^\s*(?:[-*+]\s+|\d+\.\s+|#{1,6}\s+|>\s*|\|)`)
)

// estimateRowLines is a row's height in message lines. A user message is
// shown as typed (pre-wrap): each source line wraps at cols. An agent message
// is markdown: consecutive prose lines are one paragraph, which wraps as a
// whole (a newline inside a paragraph is a space); a blank line is a
// paragraph gap of a third of a line; a list item, heading, quote or table
// row starts its own line; a link shows only its text; a fenced code block
// shows at most warmFillCodeBlockLines lines.
func estimateRowLines(item threadindex.TranscriptItem, cols int) float64 {
	if item.Role == "user" {
		total := 0.0
		for _, line := range strings.Split(item.Text, "\n") {
			total += wrappedLines(line, cols)
		}
		return total + warmFillUserChrome
	}
	total := 0.0
	paragraph := 0 // characters of the open paragraph
	flush := func() {
		if paragraph > 0 {
			total += math.Ceil(float64(paragraph) / float64(cols))
			paragraph = 0
		}
	}
	inCode, codeLines := false, 0
	for _, line := range strings.Split(item.Text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			flush()
			if inCode {
				total += float64(min(codeLines, warmFillCodeBlockLines))
				codeLines = 0
			}
			inCode = !inCode
			continue
		}
		if inCode {
			codeLines++
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
			total += 1.0 / 3
			continue
		}
		line = markdownLinkTarget.ReplaceAllString(line, "]")
		if markdownBlockStart.MatchString(line) {
			flush()
			paragraph = utf8.RuneCountInString(markdownBlockStart.ReplaceAllString(line, ""))
			continue
		}
		if paragraph > 0 {
			paragraph++ // the newline renders as a space
		}
		paragraph += utf8.RuneCountInString(line)
	}
	flush()
	if inCode {
		total += float64(min(codeLines, warmFillCodeBlockLines))
	}
	return total + warmFillAgentChrome
}

// wrappedLines is how many lines one source line takes when wrapped at cols:
// at least one, a blank line included.
func wrappedLines(line string, cols int) float64 {
	return math.Max(1, math.Ceil(float64(utf8.RuneCountInString(line))/float64(cols)))
}
