package translator

import "testing"

func TestOutputLimiterInactiveByDefault(t *testing.T) {
	limiter := &OutputLimiter{}
	if limiter.Active() {
		t.Fatal("zero value must impose no limit")
	}
	text, done := limiter.Feed("anything at all")
	if text != "anything at all" || done {
		t.Fatalf("Feed = %q, %v", text, done)
	}
	if _, _, ok := limiter.StopReason(); ok {
		t.Fatal("no limit was reached")
	}
}

func TestOutputLimiterStopSequenceTruncates(t *testing.T) {
	limiter := &OutputLimiter{StopSequences: []string{"STOP"}}
	text := limiter.Truncate("keep this STOP drop this")
	if text != "keep this " {
		t.Fatalf("Truncate = %q", text)
	}
	reason, sequence, ok := limiter.StopReason()
	if !ok || reason != "stop_sequence" || sequence != "STOP" {
		t.Fatalf("StopReason = %q, %q, %v", reason, sequence, ok)
	}
}

// A stop sequence split across deltas is the case a naive per-chunk search
// misses, so the limiter withholds a trailing window until it can decide.
func TestOutputLimiterStopSequenceAcrossDeltas(t *testing.T) {
	limiter := &OutputLimiter{StopSequences: []string{"STOP"}}

	first, done := limiter.Feed("keep ST")
	if done {
		t.Fatal("must not stop on a partial match")
	}
	second, done := limiter.Feed("OP drop")
	if !done {
		t.Fatal("completed stop sequence must end the turn")
	}
	if got := first + second; got != "keep " {
		t.Fatalf("emitted %q, want %q", got, "keep ")
	}
	if _, sequence, _ := limiter.StopReason(); sequence != "STOP" {
		t.Fatalf("stop sequence = %q", sequence)
	}
}

// Text withheld while watching for a stop sequence must still be delivered
// when the turn ends without matching one.
func TestOutputLimiterFlushReleasesWithheldText(t *testing.T) {
	limiter := &OutputLimiter{StopSequences: []string{"STOP"}}
	emitted, done := limiter.Feed("tail ST")
	if done {
		t.Fatal("partial match must not stop the turn")
	}
	if got := emitted + limiter.Flush(); got != "tail ST" {
		t.Fatalf("emitted %q, want full text", got)
	}
	if _, _, ok := limiter.StopReason(); ok {
		t.Fatal("no stop sequence was completed")
	}
}

func TestOutputLimiterMaxTokensClips(t *testing.T) {
	limiter := &OutputLimiter{MaxTokens: 2}
	long := "This sentence is far longer than two tokens worth of output."
	text := limiter.Truncate(long)
	if text == long {
		t.Fatal("text was not clipped")
	}
	if got := EstimateTokens(text); got > 2 {
		t.Fatalf("clipped text estimates %d tokens, want <= 2", got)
	}
	reason, sequence, ok := limiter.StopReason()
	if !ok || reason != "max_tokens" {
		t.Fatalf("StopReason = %q, %v", reason, ok)
	}
	if sequence != "" {
		t.Fatalf("max_tokens must not report a stop sequence, got %q", sequence)
	}
}

func TestOutputLimiterMaxTokensAllowsShortOutput(t *testing.T) {
	limiter := &OutputLimiter{MaxTokens: 1000}
	text := limiter.Truncate("short answer")
	if text != "short answer" {
		t.Fatalf("Truncate = %q", text)
	}
	if _, _, ok := limiter.StopReason(); ok {
		t.Fatal("ceiling was not reached")
	}
}

func TestOutputLimiterStopSequenceWinsOverCeiling(t *testing.T) {
	limiter := &OutputLimiter{MaxTokens: 1000, StopSequences: []string{"4"}}
	text := limiter.Truncate("1\n2\n3\n4\n5\n")
	if text != "1\n2\n3\n" {
		t.Fatalf("Truncate = %q", text)
	}
	if reason, _, _ := limiter.StopReason(); reason != "stop_sequence" {
		t.Fatalf("stop reason = %q", reason)
	}
}
