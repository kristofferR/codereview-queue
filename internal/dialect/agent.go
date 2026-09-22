package dialect

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AgentFailure describes a provider/model failure that should be retried on a
// fallback rather than counted as a failed attempt to change code.
type AgentFailure struct {
	Unavailable bool
	RetryAt     time.Time
	Reason      string
}

// ClassifyAgentFailure recognizes machine-readable outage responses emitted by
// supported coding agents. The transcript also contains repository-controlled
// command output, test failures, source code, and the review findings
// themselves, so free-text markers are not trustworthy evidence. Unknown
// failures stay ordinary fix failures: refunding a genuine bad fix forever
// would remove the loop's safety bound.
func ClassifyAgentFailure(log []byte, now time.Time) AgentFailure {
	found := false
	usageLimited := false
	var resetAt int64
	for _, line := range strings.Split(string(log), "\n") {
		var event agentEvent
		if json.Unmarshal([]byte(line), &event) != nil || !event.providerUnavailable() {
			continue
		}
		found = true
		usageLimited = usageLimited || event.codexUsageLimitMessage() != ""
		if unix, ok := event.resetUnix(now); ok {
			resetAt = unix
		}
	}
	if !found {
		return AgentFailure{}
	}

	retryAt := now.UTC().Add(15 * time.Minute)
	if resetAt != 0 {
		retryAt = time.Unix(resetAt, 0).UTC()
	}
	if !retryAt.After(now) {
		retryAt = now.UTC().Add(5 * time.Minute)
	}
	// Subscription capacity can return early through an unscheduled reset,
	// banked reset, or added credits. Probe periodically instead of treating
	// the advertised reset as a hard gate for days.
	if probeAt := now.UTC().Add(15 * time.Minute); usageLimited && retryAt.After(probeAt) {
		retryAt = probeAt
	}
	// Agent output is influenced by repository code. Never let a bogus reset
	// timestamp park a model for this head indefinitely.
	if latest := now.UTC().Add(7 * 24 * time.Hour); retryAt.After(latest) {
		retryAt = latest
	}
	return AgentFailure{
		Unavailable: true,
		RetryAt:     retryAt,
		Reason:      "provider or model temporarily unavailable",
	}
}

// agentEvent is deliberately only the outer protocol envelope. Repository
// output is embedded inside assistant/user events and may itself contain
// outage-shaped JSON; recursively scanning it would let a test refund a failed
// fix and bypass the per-head attempt bound.
type agentEvent struct {
	Type     string          `json:"type"`
	Message  string          `json:"message"`
	Error    json.RawMessage `json:"error"`
	ResetsAt json.RawMessage `json:"resetsAt"`
}

func (e agentEvent) providerUnavailable() bool {
	if e.codexUsageLimitMessage() != "" {
		return true
	}
	if e.Type != "result" && e.Type != "error" {
		return false
	}
	var direct string
	if json.Unmarshal(e.Error, &direct) == nil {
		return strings.EqualFold(direct, "rate_limit")
	}
	var nested struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Error, &nested) != nil {
		return false
	}
	return strings.EqualFold(nested.Type, "rate_limit_error") ||
		strings.EqualFold(nested.Type, "overloaded_error")
}

func (e agentEvent) resetUnix(now time.Time) (int64, bool) {
	var number json.Number
	if json.Unmarshal(e.ResetsAt, &number) == nil {
		unix, err := strconv.ParseInt(string(number), 10, 64)
		return unix, err == nil
	}
	var text string
	if json.Unmarshal(e.ResetsAt, &text) == nil {
		unix, err := strconv.ParseInt(text, 10, 64)
		return unix, err == nil
	}
	if message := e.codexUsageLimitMessage(); message != "" {
		return codexUsageReset(message, now, time.Local)
	}
	return 0, false
}

// Codex exec exposes subscription exhaustion as a message in these two
// protocol events. Never inspect assistant messages or nested tool output.
func (e agentEvent) codexUsageLimitMessage() string {
	var message string
	switch e.Type {
	case "error":
		message = e.Message
	case "turn.failed":
		var failure struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &failure) == nil {
			message = failure.Message
		}
	}
	if strings.HasPrefix(message, "You’ve hit your usage limit.") ||
		strings.HasPrefix(message, "You've hit your usage limit.") {
		return message
	}
	return ""
}

var codexResetDay = regexp.MustCompile(`\b([0-9]{1,2})(?:st|nd|rd|th),`)

// Codex formats reset times in the agent host's local timezone and omits the
// date for same-day resets. Its minute precision needs a minute of margin.
func codexUsageReset(message string, now time.Time, location *time.Location) (int64, bool) {
	_, stamp, ok := strings.Cut(strings.ToLower(message), "try again at ")
	if !ok {
		return 0, false
	}
	stamp = strings.TrimSuffix(strings.TrimSpace(stamp), ".")
	stamp = codexResetDay.ReplaceAllString(stamp, "$1,")
	stamp = strings.ToUpper(stamp)
	reset, err := time.ParseInLocation("Jan 2, 2006 3:04 PM", stamp, location)
	if err != nil {
		// Include the local date when parsing, so DST follows the reset day.
		reset, err = time.ParseInLocation("2006-01-02 3:04 PM",
			now.In(location).Format("2006-01-02")+" "+stamp, location)
	}
	if err != nil {
		return 0, false
	}
	return reset.Add(time.Minute).Unix(), true
}
