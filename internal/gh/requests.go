package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request accounting answers "what is spending the quota" from counts rather
// than from traffic volume and error logs. The account's REST allowance is
// shared by every agent and daemon, and when it ran dry the only evidence was
// what had failed, not what had been asked.

type callerKey struct{}

// WithCaller names who a request is made for, so the gateway can attribute what
// it forwards.
func WithCaller(ctx context.Context, caller string) context.Context {
	caller = strings.TrimSpace(caller)
	if caller == "" {
		return ctx
	}
	if len(caller) > 64 {
		caller = caller[:64]
	}
	return context.WithValue(ctx, callerKey{}, caller)
}

// Caller identifies this process to the gateway ("autoreview@host"). The crq
// command sets it at startup; empty sends nothing.
var Caller string

// Outcomes that never reached GitHub. Everything else is the HTTP status.
const (
	outcomeCached    = "cache"   // a git object served from memory
	outcomeThrottled = "memo"    // answered from a remembered throttle
	outcomeLimited   = "limited" // refused by GitHub for an exhausted allowance
	outcomeError     = "error"   // no response at all
)

type requestKey struct {
	caller, method, route, outcome string
}

const requestLogMinutes = 60

// maxRequestKeys bounds one minute's distinct rows. Callers name themselves, so
// the bound is what keeps a misbehaving client from growing the log without end.
const maxRequestKeys = 4096

type requestLog struct {
	mu sync.Mutex
	// self labels requests nobody else is named for: the process's own.
	self    string
	since   time.Time
	minutes [requestLogMinutes]map[requestKey]int
	stamps  [requestLogMinutes]int64
	quota   map[string]QuotaSeen
}

// QuotaSeen is an allowance as GitHub last reported it on a response.
type QuotaSeen struct {
	Resource  string    `json:"resource"`
	Limit     int       `json:"limit"`
	Used      int       `json:"used"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
	SeenAt    time.Time `json:"seen_at"`
}

// RequestRow is one caller's use of one endpoint over the window.
type RequestRow struct {
	Caller string `json:"caller"`
	Method string `json:"method"`
	Route  string `json:"route"`
	// Counted reached GitHub and drew on the allowance. Free were answered 304:
	// a conditional request GitHub does not charge for. Local never left this
	// process — a cached git object, or a throttle answered from memory.
	Counted int `json:"counted"`
	Free    int `json:"free"`
	Local   int `json:"local"`
	// Refused were turned away by GitHub for an exhausted allowance, and Failed
	// got a 5xx or no answer. Neither is charged.
	Refused int `json:"refused,omitempty"`
	Failed  int `json:"failed,omitempty"`
	// Outcomes is the same activity by status, which is what tells a write that
	// landed (200) from one that lost its compare-and-swap (422).
	Outcomes map[string]int `json:"outcomes"`
}

// RequestReport is the gateway's request accounting over a recent window.
type RequestReport struct {
	// Since is when counting began. A window reaching back past it is short.
	Since   time.Time    `json:"since"`
	Window  string       `json:"window"`
	Counted int          `json:"counted"`
	Free    int          `json:"free"`
	Local   int          `json:"local"`
	Quota   []QuotaSeen  `json:"quota"`
	Rows    []RequestRow `json:"rows"`
}

// EnableRequestLog starts counting this client's requests. crq serve turns it
// on for the one client that reaches GitHub; self names the requests that
// process makes on its own account.
func (g *GitHub) EnableRequestLog(self string) {
	g.requests = &requestLog{self: self, since: time.Now(), quota: map[string]QuotaSeen{}}
}

func (g *GitHub) countRequest(ctx context.Context, method, fullURL, outcome string) {
	log := g.requests
	if log == nil {
		return
	}
	caller, _ := ctx.Value(callerKey{}).(string)
	if caller == "" {
		caller = log.self
	}
	key := requestKey{caller: caller, method: method, route: requestRoute(fullURL), outcome: outcome}
	minute := time.Now().Unix() / 60
	slot := minute % requestLogMinutes
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.stamps[slot] != minute || log.minutes[slot] == nil {
		log.stamps[slot], log.minutes[slot] = minute, map[requestKey]int{}
	}
	if _, seen := log.minutes[slot][key]; !seen && len(log.minutes[slot]) >= maxRequestKeys {
		key = requestKey{caller: "(other)", method: method, route: "(other)", outcome: outcome}
	}
	log.minutes[slot][key]++
}

// noteQuota records the allowance a response reported.
func (g *GitHub) noteQuota(header http.Header) {
	log := g.requests
	if log == nil {
		return
	}
	resource := header.Get("X-RateLimit-Resource")
	if resource == "" {
		return
	}
	number := func(name string) int {
		n, _ := strconv.Atoi(header.Get(name))
		return n
	}
	seen := QuotaSeen{
		Resource: resource, Limit: number("X-RateLimit-Limit"), Used: number("X-RateLimit-Used"),
		Remaining: number("X-RateLimit-Remaining"), SeenAt: time.Now().UTC(),
	}
	if epoch, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		seen.Reset = time.Unix(epoch, 0).UTC()
	}
	log.mu.Lock()
	log.quota[resource] = seen
	log.mu.Unlock()
}

// Requests reports what this client asked of GitHub over the last window (at
// most an hour), busiest endpoint first.
func (g *GitHub) Requests(window time.Duration) RequestReport {
	if window <= 0 || window > requestLogMinutes*time.Minute {
		window = requestLogMinutes * time.Minute
	}
	report := RequestReport{Window: window.String(), Quota: []QuotaSeen{}, Rows: []RequestRow{}}
	log := g.requests
	if log == nil {
		return report
	}
	oldest := time.Now().Add(-window).Unix() / 60
	type rowKey struct{ caller, method, route string }
	rows := map[rowKey]*RequestRow{}
	log.mu.Lock()
	report.Since = log.since.UTC()
	for slot, counts := range log.minutes {
		if log.stamps[slot] <= oldest {
			continue
		}
		for key, n := range counts {
			id := rowKey{key.caller, key.method, key.route}
			row := rows[id]
			if row == nil {
				row = &RequestRow{Caller: key.caller, Method: key.method, Route: key.route, Outcomes: map[string]int{}}
				rows[id] = row
			}
			row.Outcomes[key.outcome] += n
			switch {
			case key.outcome == outcomeCached || key.outcome == outcomeThrottled:
				row.Local += n
			case key.outcome == strconv.Itoa(http.StatusNotModified):
				row.Free += n
			case key.outcome == outcomeLimited:
				row.Refused += n
			case key.outcome == outcomeError || strings.HasPrefix(key.outcome, "5"):
				row.Failed += n
			default:
				row.Counted += n
			}
		}
	}
	for _, seen := range log.quota {
		report.Quota = append(report.Quota, seen)
	}
	log.mu.Unlock()

	for _, row := range rows {
		report.Rows = append(report.Rows, *row)
		report.Counted += row.Counted
		report.Free += row.Free
		report.Local += row.Local
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		a, b := report.Rows[i], report.Rows[j]
		if a.Counted != b.Counted {
			return a.Counted > b.Counted
		}
		if a.Free+a.Local != b.Free+b.Local {
			return a.Free+a.Local > b.Free+b.Local
		}
		return a.Caller+a.Method+a.Route < b.Caller+b.Method+b.Route
	})
	sort.Slice(report.Quota, func(i, j int) bool { return report.Quota[i].Resource < report.Quota[j].Resource })
	return report
}

// requestRoute reduces a URL to its endpoint: numbers, hashes and the names
// under /repos and /users become placeholders, and the query is dropped, so one
// endpoint is one row however many pull requests it was asked about.
func requestRoute(fullURL string) string {
	path := fullURL
	if u, err := url.Parse(fullURL); err == nil {
		path = u.Path
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		switch {
		case i > 0 && i <= 2 && parts[0] == "repos":
			parts[i] = "*"
		case i == 1 && (parts[0] == "users" || parts[0] == "orgs" || parts[0] == "repositories"):
			parts[i] = "*"
		case i > 0 && parts[i-1] == "heads":
			// A ref name, and everything after it.
			return "/" + strings.Join(append(parts[:i:i], "*"), "/")
		case isNumberOrHash(part):
			parts[i] = "*"
		}
	}
	return "/" + strings.Join(parts, "/")
}

func isNumberOrHash(part string) bool {
	if part == "" {
		return false
	}
	digits := true
	for _, c := range part {
		if c < '0' || c > '9' {
			digits = false
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return digits || len(part) >= 7
}

// ServerRequests reads the request accounting of the crq serve at serverURL.
func ServerRequests(ctx context.Context, serverURL, token string, window time.Duration) (RequestReport, error) {
	g, err := NewGitHubViaServer(serverURL, token)
	if err != nil {
		return RequestReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := strings.TrimSuffix(g.gatewayURL, "/api/github") + "/api/requests"
	if window > 0 {
		endpoint += "?window=" + url.QueryEscape(window.String())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return RequestReport{}, err
	}
	g.decorate(req)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return RequestReport{}, fmt.Errorf("reading request accounting from crq serve: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return RequestReport{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return RequestReport{}, fmt.Errorf("crq serve answered %s for its request accounting: %s", resp.Status, snippet(body))
	}
	// A server from before the accounting has no such route and answers with
	// its dashboard page instead.
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return RequestReport{}, errors.New("this crq serve keeps no request accounting; upgrade and restart it")
	}
	var report RequestReport
	if err := json.Unmarshal(body, &report); err != nil {
		return RequestReport{}, fmt.Errorf("crq serve returned request accounting this binary cannot read: %w", err)
	}
	return report, nil
}
