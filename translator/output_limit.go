package translator

import (
	"math"
	"strings"
	"unicode/utf8"
)

// tokensPerRune mirrors the estimator behind /v1/messages/count_tokens. Cursor
// bills whole turns and never reports a running output count, so honouring
// max_tokens requires a local estimate rather than an upstream signal.
const tokensPerRune = 3.6

// EstimateTokens approximates Anthropic's tokenizer well enough to enforce a
// caller's max_tokens ceiling. It intentionally matches the count_tokens
// endpoint so a client that pre-counts sees a consistent number.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	estimate := int(math.Ceil(float64(utf8.RuneCountInString(text)) / tokensPerRune))
	if estimate < 1 {
		return 1
	}
	return estimate
}

// OutputLimiter enforces the two generation controls Cursor's protocol has no
// field for: `stop_sequences` and `max_tokens`.
//
// Cursor accepts neither, so an unrestricted turn always comes back as
// `end_turn` with whatever length the model chose. Clients that rely on either
// control — and conformance suites that assert `stop_reason` — see a contract
// violation. The limiter clips the assistant text locally and reports the stop
// reason Anthropic's API would have returned.
//
// The zero value imposes no limit. Feed must be called with successive deltas
// of one assistant turn; Truncate handles the non-streaming case where the
// whole text is known at once.
type OutputLimiter struct {
	MaxTokens     int
	StopSequences []string

	emitted      strings.Builder
	pending      string
	stopped      bool
	stopReason   string
	stopSequence string
}

// Active reports whether either control is in play, so callers can skip the
// bookkeeping entirely on the common unrestricted path.
func (l *OutputLimiter) Active() bool {
	return l != nil && (l.MaxTokens > 0 || len(l.StopSequences) > 0)
}

// Feed accepts the next text delta and returns the portion that may be sent
// downstream. done is true once a limit has been reached, after which the
// caller must stop emitting text and finish the message.
//
// A stop sequence can straddle delta boundaries, so Feed withholds a trailing
// window large enough to contain any partial match and releases it on the next
// call or from Flush.
func (l *OutputLimiter) Feed(delta string) (string, bool) {
	if !l.Active() {
		return delta, false
	}
	if l.stopped {
		return "", true
	}

	buffered := l.pending + delta
	l.pending = ""

	if index, sequence := firstStopSequence(buffered, l.StopSequences); index >= 0 {
		l.stopped = true
		l.stopReason = "stop_sequence"
		l.stopSequence = sequence
		return l.admit(buffered[:index]), true
	}

	// Hold back only as much as a stop sequence could still need.
	if hold := partialStopSuffix(buffered, l.StopSequences); hold > 0 {
		l.pending = buffered[len(buffered)-hold:]
		buffered = buffered[:len(buffered)-hold]
	}
	return l.admit(buffered), l.stopped
}

// Flush releases any text withheld while watching for a stop sequence. It is
// called when the upstream turn ends without hitting a limit.
func (l *OutputLimiter) Flush() string {
	if !l.Active() || l.stopped {
		return ""
	}
	pending := l.pending
	l.pending = ""
	return l.admit(pending)
}

// admit applies the max_tokens ceiling to text already cleared by the stop
// sequence check.
func (l *OutputLimiter) admit(text string) string {
	if text == "" {
		return ""
	}
	if l.MaxTokens <= 0 {
		l.emitted.WriteString(text)
		return text
	}
	remaining := l.MaxTokens - EstimateTokens(l.emitted.String())
	if remaining <= 0 {
		l.stopped = true
		l.stopReason = "max_tokens"
		return ""
	}
	if EstimateTokens(l.emitted.String()+text) <= l.MaxTokens {
		l.emitted.WriteString(text)
		return text
	}

	clipped := clipToTokenBudget(l.emitted.String(), text, l.MaxTokens)
	l.stopped = true
	l.stopReason = "max_tokens"
	l.emitted.WriteString(clipped)
	return clipped
}

// Truncate applies both limits to a complete assistant turn and reports the
// resulting stop reason. It is the non-streaming counterpart to Feed.
func (l *OutputLimiter) Truncate(text string) string {
	if !l.Active() {
		return text
	}
	out, _ := l.Feed(text)
	return out + l.Flush()
}

// StopReason returns the Anthropic stop reason a limit produced. ok is false
// when no limit was reached, leaving the upstream reason in place.
func (l *OutputLimiter) StopReason() (reason string, sequence string, ok bool) {
	if l == nil || !l.stopped || l.stopReason == "" {
		return "", "", false
	}
	return l.stopReason, l.stopSequence, true
}

// EmittedTokens is the estimated output token count after clipping, so usage
// stays consistent with the text the caller actually received.
func (l *OutputLimiter) EmittedTokens() int {
	if l == nil {
		return 0
	}
	return EstimateTokens(l.emitted.String())
}

func firstStopSequence(text string, sequences []string) (int, string) {
	best, match := -1, ""
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if index := strings.Index(text, sequence); index >= 0 && (best < 0 || index < best) {
			best, match = index, sequence
		}
	}
	return best, match
}

// partialStopSuffix returns how many trailing bytes of text could still grow
// into a stop sequence on the next delta.
func partialStopSuffix(text string, sequences []string) int {
	longest := 0
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		limit := len(sequence) - 1
		if limit > len(text) {
			limit = len(text)
		}
		for size := limit; size > longest; size-- {
			if strings.HasPrefix(sequence, text[len(text)-size:]) {
				longest = size
				break
			}
		}
	}
	return longest
}

// clipToTokenBudget grows the addition rune by rune until one more rune would
// exceed the ceiling, so the cut lands on a character boundary.
func clipToTokenBudget(existing, addition string, maxTokens int) string {
	accepted := 0
	for index := range addition {
		if index == 0 {
			continue
		}
		if EstimateTokens(existing+addition[:index]) > maxTokens {
			break
		}
		accepted = index
	}
	if accepted == 0 && EstimateTokens(existing+addition) <= maxTokens {
		return addition
	}
	return addition[:accepted]
}
