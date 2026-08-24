package crq

import (
	"context"
	"fmt"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
)

// CostEstimate is one reviewer's price for one head, as JSON the dashboard and
// any script can read. It mirrors dialect.CostEstimate rather than reusing it so
// the wire shape is owned here, where the rest of the CLI's shapes live.
type CostEstimate struct {
	Bot  string  `json:"bot"`
	Low  float64 `json:"low"`
	High float64 `json:"high"`
	// Metered says this review spends CodeRabbit's account allowance.
	Metered bool `json:"metered"`
	// Exact says low and high are the same figure and it is not a guess.
	Exact bool `json:"exact,omitempty"`
	// Unknown says crq has no basis to price this reviewer. Renderers must show
	// that rather than $0.00, which reads as free.
	Unknown bool   `json:"unknown,omitempty"`
	Basis   string `json:"basis"`
}

// RoundCost is what one more review round on a pull request would cost.
//
// Everything here is an ESTIMATE and the shape says so: a range, a per-reviewer
// breakdown that keeps its own reasoning, and the date the prices behind it were
// last checked against the vendors. A single confident figure would be the one
// output guaranteed to be wrong.
type RoundCost struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	Head string `json:"head"`

	Low  float64 `json:"low"`
	High float64 `json:"high"`
	// Exact is true only when every reviewer's figure is exact.
	Exact bool `json:"exact,omitempty"`
	// Unpriced names reviewers crq could not price at all, so a reader knows the
	// total is a floor rather than an answer.
	Unpriced []string `json:"unpriced,omitempty"`

	Diff struct {
		Additions    int `json:"additions"`
		Deletions    int `json:"deletions"`
		ChangedFiles int `json:"changed_files"`
	} `json:"diff"`

	Reviewers []CostEstimate `json:"reviewers"`
	// Summary is the one line a UI can show without unpacking anything.
	Summary string `json:"summary"`
	// PricesCheckedAt is when the published figures behind this were verified.
	PricesCheckedAt string `json:"prices_checked_at"`
	// PricingNote is the vendor-owned billing disclosure from dialect.
	PricingNote string `json:"pricing_note"`
}

// Cost estimates the next round on repo#pr, using the repository's effective
// reviewers and the account's remaining allowance.
//
// It costs one pull-request read: the diff stat comes from the same object crq
// already fetches to learn the head, so this is not a new class of expense — but
// it IS one call per pull request, which is why the overview does not show a
// cost per queue row and this is asked for a PR at a time.
func (s *Service) Cost(ctx context.Context, repo string, pr int) (RoundCost, error) {
	repo = NormalizeRepo(repo)
	if err := checkRepoShape(repo); err != nil {
		return RoundCost{}, err
	}
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return RoundCost{}, err
	}
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return RoundCost{}, err
	}
	return s.costFrom(st, repo, pr, pull.Head.SHA, dialect.DiffStat{
		Additions:    pull.Additions,
		Deletions:    pull.Deletions,
		ChangedFiles: pull.ChangedFiles,
	}), nil
}

// costFrom is the pure half, so a caller that already holds the state and the
// diff stat does not pay for either again.
func (s *Service) costFrom(st State, repo string, pr int, head string, d dialect.DiffStat) RoundCost {
	return s.costWith(st, repo, pr, head, d, accountAllowance(st))
}

// accountAllowance is what the account last told crq about its included
// reviews. Absent stays absent: RemainingKnown false is "crq has not seen a
// count", which prices differently from having seen a zero.
func accountAllowance(st State) dialect.Allowance {
	a := dialect.Allowance{}
	if st.Account.Remaining != nil {
		a.Remaining, a.RemainingKnown = *st.Account.Remaining, true
	}
	return a
}

// costWith prices one head against an allowance the caller supplies, so a
// caller pricing a BACKLOG can spend it down across the pull requests. Pricing
// every one of them against the same unchanged count reported a 20-deep backlog
// as entirely included when the account had one review left.
func (s *Service) costWith(st State, repo string, pr int, head string, d dialect.DiffStat, allowance dialect.Allowance) RoundCost {
	cfg := s.cfgFor(st, repo)
	out := RoundCost{
		Repo: repo, PR: pr, Head: head, Exact: true,
		PricesCheckedAt: dialect.PricesCheckedAt, PricingNote: dialect.PricingDisclosure,
	}
	out.Diff.Additions, out.Diff.Deletions, out.Diff.ChangedFiles = d.Additions, d.Deletions, d.ChangedFiles

	// Usage-based billing is only ever learned from the CLI's own guidance, and
	// crq does not persist it yet — so it stays UNKNOWN rather than "off". The
	// two are not interchangeable: "off" is a claim that an exhausted allowance
	// costs nothing, and stating it on no evidence would show an account with
	// overages enabled "no per-review cost" for a backlog it is about to be
	// billed for. Unknown says so instead of guessing.
	for _, r := range cfg.Reviewers {
		est := dialect.EstimateCost(r.Login, cfg.Bot, d, allowance)
		// A non-primary reviewer that crq does not always command may review
		// automatically, or may not participate in this round at all. Its
		// published price is therefore an upper bound, never a guaranteed
		// minimum.
		if !est.Metered && r.Trigger != engine.TriggerAlways {
			est.Low = 0
			if est.High > 0 {
				est.Exact = false
			}
			est.Basis = "may not participate; " + est.Basis
		}
		out.Reviewers = append(out.Reviewers, CostEstimate{
			Bot: est.Bot, Low: est.Low, High: est.High,
			Metered: est.Metered, Exact: est.Exact, Unknown: est.Unknown, Basis: est.Basis,
		})
		if est.Unknown {
			out.Unpriced = append(out.Unpriced, r.Login)
			out.Exact = false
			continue
		}
		out.Low += est.Low
		out.High += est.High
		if !est.Exact {
			out.Exact = false
		}
	}
	out.Summary = costSummary(out)
	return out
}

// costSummary renders the figure a person reads first. It never rounds an
// uncertainty away: a range stays a range, and an incomplete total says so.
func costSummary(c RoundCost) string {
	if len(c.Reviewers) > 0 && len(c.Unpriced) == len(c.Reviewers) {
		return fmt.Sprintf("no basis to price %d reviewer(s)", len(c.Unpriced))
	}
	var figure string
	switch {
	case c.Exact && c.High == 0:
		figure = "no per-review cost"
	case c.Low == c.High:
		figure = fmt.Sprintf("about $%.2f", c.High)
	default:
		figure = fmt.Sprintf("$%.2f–$%.2f", c.Low, c.High)
	}
	if len(c.Unpriced) > 0 {
		return figure + fmt.Sprintf(", plus %d reviewer(s) crq cannot price", len(c.Unpriced))
	}
	if c.Exact {
		return figure
	}
	return "estimated " + figure
}
