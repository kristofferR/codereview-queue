package gh

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The accounting has to tell a request that cost quota from one that did not,
// and say who asked — that is the whole of what an exhausted quota needs
// explained.
func TestRequestLogSeparatesCountedFreeAndLocalByCaller(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Used", "41")
		w.Header().Set("X-RateLimit-Remaining", "4959")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		switch {
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			_, _ = w.Write([]byte(`{"sha":"` + sha + `","encoding":"utf-8","content":"state"}`))
		case r.Method == http.MethodPatch:
			http.Error(w, `{"message":"Update is not a fast forward"}`, http.StatusUnprocessableEntity)
		case r.Header.Get("If-None-Match") != "":
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write([]byte(`{"number":7}`))
		}
	}))
	defer srv.Close()
	g := NewTestClient(srv.URL, srv.Client())
	g.EnableRequestLog("serve")

	autofix := WithCaller(t.Context(), "autofix@host")
	for range 3 { // one counted fetch, then two free revalidations
		if _, err := g.GetPull(autofix, "o/r", 7); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 { // fetched once, then served from memory
		if _, err := g.GetBlob(t.Context(), "o/state", sha); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.UpdateRef(autofix, "o/state", "crq-state-v3", sha, false); err == nil {
		t.Fatal("the fake refuses every ref update")
	}

	report := g.Requests(time.Hour)
	if report.Counted != 3 || report.Free != 2 || report.Local != 1 {
		t.Fatalf("totals = %d counted, %d free, %d local; want 3, 2, 1", report.Counted, report.Free, report.Local)
	}
	// Counts are kept by the minute, so a shorter window still covers the
	// minute it falls in rather than nothing.
	if recent := g.Requests(30 * time.Second); recent.Counted != 3 {
		t.Fatalf("a 30s window counted %d, want the requests just made", recent.Counted)
	}
	rows := map[string]RequestRow{}
	for _, row := range report.Rows {
		rows[row.Caller+" "+row.Method+" "+row.Route] = row
	}
	if row := rows["autofix@host GET /repos/*/*/pulls/*"]; row.Counted != 1 || row.Free != 2 {
		t.Errorf("pull reads = %+v, want 1 counted and 2 free", row)
	}
	if row := rows["serve GET /repos/*/*/git/blobs/*"]; row.Counted != 1 || row.Local != 1 {
		t.Errorf("blob reads = %+v, want 1 counted and 1 local, attributed to the server itself", row)
	}
	// A lost compare-and-swap is told apart from a landed one by its status.
	if row := rows["autofix@host PATCH /repos/*/*/git/refs/heads/*"]; row.Counted != 1 || row.Outcomes["422"] != 1 {
		t.Errorf("ref update = %+v, want one counted 422", row)
	}
	if len(report.Quota) != 1 || report.Quota[0].Resource != "core" || report.Quota[0].Used != 41 || !report.Quota[0].Reset.Equal(reset) {
		t.Errorf("quota = %+v, want the allowance the responses reported", report.Quota)
	}
}

func TestRequestLogCountsARefusalApartFromAPermissionError(t *testing.T) {
	upstream := &exhaustedUpstream{reset: time.Now().Add(20 * time.Minute)}
	g := gatewayClient(t, upstream)
	g.EnableRequestLog("serve")
	for range 3 {
		if _, err := g.GetPull(t.Context(), "o/r", 7); !IsThrottled(err) {
			t.Fatalf("err = %v, want a throttle", err)
		}
	}
	report := g.Requests(time.Hour)
	if len(report.Rows) != 1 || report.Counted != 0 {
		t.Fatalf("report = %+v, want one row and nothing counted", report)
	}
	// One request reached GitHub and was refused; the rest were answered from
	// the remembered throttle and never left.
	if row := report.Rows[0]; row.Refused != 1 || row.Local != 2 {
		t.Fatalf("row = %+v, want 1 refused and 2 local", row)
	}
}

func TestRequestRoute(t *testing.T) {
	for url, want := range map[string]string{
		"https://api.github.com/repos/kristofferR/crq-state/git/ref/heads/crq-state-v3":                "/repos/*/*/git/ref/heads/*",
		"https://api.github.com/repos/o/r/git/refs/heads/feature/x":                                    "/repos/*/*/git/refs/heads/*",
		"https://api.github.com/repos/o/r/pulls/12/reviews?per_page=100":                               "/repos/*/*/pulls/*/reviews",
		"https://api.github.com/repos/o/r/commits/0123456789abcdef0123456789abcdef01234567/check-runs": "/repos/*/*/commits/*/check-runs",
		"https://api.github.com/search/issues?q=type%3Apr":                                             "/search/issues",
		"https://api.github.com/users/kristofferR":                                                     "/users/*",
		"https://api.github.com/graphql":                                                               "/graphql",
	} {
		if got := requestRoute(url); got != want {
			t.Errorf("requestRoute(%s) = %q, want %q", url, got, want)
		}
	}
}
