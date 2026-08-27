package crq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

type FeedbackReport struct {
	Status     string            `json:"status"`
	Repo       string            `json:"repo"`
	PR         int               `json:"pr"`
	Head       string            `json:"head"`
	Reason     string            `json:"reason,omitempty"`
	Converged  bool              `json:"converged"`
	ReviewedBy map[string]bool   `json:"reviewed_by"`
	Findings   []dialect.Finding `json:"findings"`
	// Dismissed counts the findings this head's round accounted for through
	// `crq dismiss`. They are withheld from Findings, so the count is how a
	// reader sees something was set aside rather than never reported.
	Dismissed int       `json:"dismissed,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	// HeadOID and Diff come from the same pull read as Head. They stay outside
	// the frozen feedback JSON contract, but let an in-process dashboard price
	// the observed head without repeating that pull read.
	HeadOID string           `json:"-"`
	Diff    dialect.DiffStat `json:"-"`
	// Open is whether the PR was open in the same observation Head and
	// ReviewedBy came from. It is deliberately not serialized — the feedback
	// JSON contract is frozen — and exists so a caller deciding an action reads
	// head, open and evidence from ONE snapshot rather than re-reading the pull.
	Open bool `json:"-"`
	// HeadRef and HeadRepo describe where the PR's branch actually lives. On a
	// fork PR that repository is not the one the PR is filed against, and a
	// contributor's checkout only has a remote for it. Not serialized: the
	// feedback JSON contract is frozen.
	HeadRef  string `json:"-"`
	HeadRepo string `json:"-"`
	// LastEvidenceAt is when the newest review from a feedback bot landed. It
	// anchors the settle window for a caller that holds no state between calls:
	// "quiet since" is derivable, where the loop's in-process settledAt is not.
	LastEvidenceAt time.Time `json:"-"`
	// CodeRabbitDeferred marks a round degraded to Codex-only while the
	// CodeRabbit account is rate-limited: Codex feedback is authoritative for
	// this round, the CodeRabbit review stays queued and fires after
	// DeferredUntil. Converged stays false until it does.
	CodeRabbitDeferred bool       `json:"coderabbit_deferred,omitempty"`
	DeferredUntil      *time.Time `json:"coderabbit_deferred_until,omitempty"`
	// CoReviewers is each enabled co-reviewer's per-head status, keyed by
	// normalized login. Informational only — the Verdict never gates
	// convergence or changes exit codes.
	CoReviewers map[string]CoReviewerStatus `json:"co_reviewers,omitempty"`
	// PrimaryUnavailable reports that the configured reviewer will not review
	// this head at all (its plan only summarizes, or it skipped the head), with
	// PrimaryUnavailableReason saying which. The co-reviewers alone resolve the
	// round: do NOT hold the head for the primary, and ignore the account-quota
	// window entirely — it cannot apply to a round that never spends quota.
	PrimaryUnavailable       bool   `json:"primary_review_unavailable,omitempty"`
	PrimaryUnavailableReason string `json:"primary_review_unavailable_reason,omitempty"`
	// PrimaryAckPending reports that the round still holds the fire slot for a
	// review command the primary has not acknowledged. A required set that omits
	// the primary converges without it, so the loop must not read convergence as
	// permission to release the slot. Not serialized: the feedback JSON contract
	// is frozen.
	PrimaryAckPending bool `json:"-"`
	// config is the reviewer configuration identity that produced this report.
	// A completion write revalidates it under CAS so an override committed after
	// observation cannot turn stale convergence into a permanent dedup marker.
	config Config
	// onePassReviewed is the campaign's PR-wide evidence predicate evaluated
	// from this report's single GitHub observation. It is deliberately not part
	// of the stable feedback JSON contract.
	onePassReviewed bool
}

// CoReviewerStatus is one co-reviewer's observed state for the current head.
type CoReviewerStatus struct {
	// Reviewed reports head review evidence: a review, a SHA-matched clean
	// summary, or a completed check run.
	Reviewed bool `json:"reviewed"`
	// CheckState summarizes the bot's check runs for the head:
	// clean | issues | in_progress | failed (the run crashed) | unable (the bot
	// reported it cannot review this commit at all) | unknown (no check observed).
	CheckState string `json:"check_state,omitempty"`
	// Verdict is Macroscope's approvability verdict when one was posted:
	// approved | needs_human_review.
	Verdict string `json:"verdict,omitempty"`
}

func (s *Service) Feedback(ctx context.Context, repo string, pr int) (FeedbackReport, error) {
	return s.feedback(ctx, repo, pr, true)
}

// FeedbackReadOnly observes a pull request without persisting incidental
// account-quota evidence. It is the dashboard's read-only path.
func (s *Service) FeedbackReadOnly(ctx context.Context, repo string, pr int) (FeedbackReport, error) {
	return s.feedback(ctx, repo, pr, false)
}

// FeedbackIn is Feedback against state a persistent caller already loaded. It
// retains Feedback's incidental evidence writes while avoiding a redundant
// state-ref read before the observation.
func (s *Service) FeedbackIn(ctx context.Context, st State, repo string, pr int) (FeedbackReport, error) {
	return s.feedbackIn(ctx, st, repo, pr, true)
}

// FeedbackReadOnlyIn observes a pull request against state the caller already
// loaded. Persistent callers such as crq serve must not pay for the four-step
// state-ref read a second time merely to render one PR.
func (s *Service) FeedbackReadOnlyIn(ctx context.Context, st State, repo string, pr int) (FeedbackReport, error) {
	return s.feedbackIn(ctx, st, repo, pr, false)
}

// onePassReviewEvidence answers the campaign's PR-wide "has any configured
// reviewer genuinely completed a review?" question from Feedback's existing
// observation. It deliberately mirrors reviewNeeded's anyConfigured branch,
// but performs no GitHub reads: reviews and comments span the PR, completed
// check runs cover the observed head, and campaign evidence is the durable
// record for a check/comment answer or removed reviewer on an older head.
func onePassReviewEvidence(st State, cfg Config, repo string, pr int, obs observation) bool {
	campaign := st.EffectiveSolver(repo).OnePassCampaign
	if st.OnePassReviewed(repo, pr, campaign) || st.OnePassReviewerAnswered(repo, pr, campaign) {
		return true
	}
	configured := onePassReviewerScope(st, cfg, repo, campaign)

	primary := dialect.NormalizeBotName(cfg.Bot)
	for _, review := range obs.reviews {
		login := dialect.NormalizeBotName(review.User.Login)
		if review.CommitID == "" || strings.EqualFold(review.State, "PENDING") {
			continue
		}
		if cfg.isConfiguredBot(review.User.Login) &&
			strings.EqualFold(review.State, "COMMENTED") && strings.TrimSpace(review.Body) == "" {
			continue
		}
		if configured[login] {
			return true
		}
	}

	for _, check := range obs.eng.Checks {
		if configured[dialect.NormalizeBotName(check.Bot)] &&
			(check.Verdict == dialect.CheckDone || check.Verdict == dialect.CheckDoneClean) {
			return true
		}
	}
	for _, event := range obs.eng.Events {
		login := dialect.NormalizeBotName(event.Bot)
		if configured[login] && login == primary &&
			event.Kind == dialect.EvNoAction && !event.SummaryOnly {
			return true
		}
		if configured[login] && event.Kind == dialect.EvCoClean {
			return true
		}
	}
	return false
}

func (s *Service) feedback(ctx context.Context, repo string, pr int, persist bool) (FeedbackReport, error) {
	repo = NormalizeRepo(repo)
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return FeedbackReport{}, err
	}
	return s.feedbackIn(ctx, st, repo, pr, persist)
}

func (s *Service) feedbackIn(ctx context.Context, st State, repo string, pr int, persist bool) (FeedbackReport, error) {
	repo = NormalizeRepo(repo)
	now := s.clock()
	round := st.Round(repo, pr)

	// One fetch drives both halves: observe() reads the pull, reviews and issue
	// comments (plus reactions when the round has fired). Feedback parses its
	// findings from the raw reviews/comments and derives convergence from
	// engine.Completion over the same snapshot — no second fetch path, and the
	// "is head reviewed?" rules live only in the engine.
	cfg := s.cfgFor(st, repo)
	obs, err := s.observe(ctx, cfg, repo, pr, round, collectPosted(st, repo, pr).commands, now)
	if err != nil {
		return FeedbackReport{}, err
	}

	// Review threads are independent of the state work below, but only useful
	// after the required REST observation succeeds. Starting here preserves
	// that overlap without spending a GraphQL read on a failed observation.
	type threadsRead struct {
		threads []reviewThread
		err     error
	}
	threadCtx, cancelThreads := context.WithCancel(ctx)
	defer cancelThreads()
	threadsCh := make(chan threadsRead, 1)
	go func() {
		threads, err := s.reviewThreads(threadCtx, repo, pr)
		threadsCh <- threadsRead{threads: threads, err: err}
	}()
	pull := obs.pull
	head := ""
	if len(pull.Head.SHA) >= 9 {
		head = pull.Head.SHA[:9]
	}
	// A rate-limit notice is evidence about the ACCOUNT, and this is the only
	// place that looks at a PR the queue is not about to fire. Pump records the
	// notice on the round it selects; a notice sitting on a PR that was
	// superseded — or is simply not next in the queue — was seen here and thrown
	// away, and the next fire went out inside a window the bot had already
	// stated. It is the one write on this path, it happens once per notice rather
	// than once per poll, and all it can do is stop a review.
	if persist {
		if updated, err := s.recordObservedBlock(ctx, cfg, obs, st, now); err != nil {
			return FeedbackReport{}, fmt.Errorf("recording the account block observed on %s: %w", QueueKey(repo, pr), err)
		} else if updated != nil {
			st = *updated
		}
		// Feedback can be the only path that observes a co-reviewer's response
		// before the loop creates or completes the round. Persist the same durable
		// activity proof Pump records so a subsequent head can self-heal a missed
		// automatic review. A preview supplies the identity needed to write the
		// per-PR activity index without publishing a fire-eligible round.
		observedRound := round
		if observedRound == nil {
			preview := st.PreviewRound(repo, pr, head, now)
			observedRound = &preview
		}
		updated, err := s.noteCoAnswers(ctx, cfg, *observedRound, obs.eng, now)
		if err != nil {
			return FeedbackReport{}, fmt.Errorf("recording reviewer answers for %s: %w", QueueKey(repo, pr), err)
		}
		if updated != nil {
			// noteCoAnswers can add the first durable proof that a self-heal
			// reviewer is active. Use the post-update state for completion so the
			// same observation cannot converge past the reviewer it just restored.
			st = *updated
			round = st.Round(repo, pr)
		}
	}
	onePassReviewed := onePassReviewEvidence(st, cfg, repo, pr, obs)
	if persist && cfg.OnePass && onePassReviewed {
		campaign := st.EffectiveSolver(repo).OnePassCampaign
		updated, current, err := s.markOnePassReviewed(ctx, st, cfg, repo, pr, campaign, now)
		if err != nil {
			return FeedbackReport{}, fmt.Errorf("recording one-pass review evidence for %s: %w", QueueKey(repo, pr), err)
		}
		if current {
			st = updated
		} else {
			// A policy change raced this observation. The next pass will classify
			// the same GitHub data against the new campaign scope.
			onePassReviewed = false
		}
	}
	report := FeedbackReport{
		Status:     "feedback",
		Repo:       repo,
		PR:         pr,
		Head:       head,
		Open:       obs.eng.Open,
		HeadRef:    pull.Head.Ref,
		HeadRepo:   NormalizeRepo(pull.Head.Repo.FullName),
		ReviewedBy: map[string]bool{},
		config:     cfg,
		Findings:   []dialect.Finding{},
		CheckedAt:  now,
		HeadOID:    pull.Head.SHA,
		Diff: dialect.DiffStat{
			Additions: pull.Additions, Deletions: pull.Deletions, ChangedFiles: pull.ChangedFiles,
		},
		onePassReviewed: onePassReviewed,
	}

	// The completion anchor is the current round only when it still tracks this
	// head. Before enqueue, preview the replacement round so durable activity
	// from a silent co-reviewer still gates the first decision for this head.
	// Otherwise Next can decide "done", enqueue a round that restores that
	// activity, park it awaiting the co-reviewer, and still return the stale
	// pre-enqueue verdict.
	completionRound := st.PreviewRound(repo, pr, head, now)
	if round != nil && round.Head == head {
		completionRound = *round
	}
	anchorOK := completionRound.FiredAt != nil
	anchorCutoff := time.Time{}
	if anchorOK {
		anchorCutoff = completionRound.FiredAt.UTC()
	}
	completion := engine.Completion(completionRound, obs.eng, cfg.policy())
	report.ReviewedBy = completion.ReviewedBy
	report.PrimaryAckPending = engine.PrimaryAckPending(completionRound, obs.eng, cfg.policy())
	// Completion does not always arrive as a review: a clean-summary comment, a
	// paired completion reply and a co-reviewer check run all satisfy it. Anchor
	// the settle window on the newest of ANY of them, or a round completed by a
	// comment would settle against a stale timestamp and converge instantly.
	evidenceBots := cfg.evidenceBots()
	noteEvidence := func(at time.Time) {
		if at.After(report.LastEvidenceAt) {
			report.LastEvidenceAt = at
		}
	}
	for _, review := range obs.reviews {
		if dialect.InBots(evidenceBots, review.User.Login) {
			noteEvidence(review.SubmittedAt)
		}
	}
	for _, comment := range obs.comments {
		if dialect.InBots(evidenceBots, comment.User.Login) {
			noteEvidence(comment.UpdatedAt)
		}
	}
	for _, check := range obs.eng.Checks {
		noteEvidence(check.CompletedAt)
	}
	verdictCutoff := anchorCutoff
	if verdictCutoff.IsZero() {
		// Not fired yet (queued behind another PR): without a fire anchor the
		// newest verdict on the PR describes the PREVIOUS head. Bound it by the
		// head commit instead of reporting stale state as current.
		verdictCutoff = obs.eng.HeadAt
	}
	report.CoReviewers = coReviewerStatuses(cfg, obs.eng, verdictCutoff)
	if why := engine.PrimaryUnavailableReason(obs.eng, cfg.policy(), head); why != "" {
		report.PrimaryUnavailable = true
		report.PrimaryUnavailableReason = cfg.Bot + " " + why
	}

	// extractBots is the broader set whose findings we surface — a superset that
	// includes Codex — so a bot that reviews without being required (and would
	// hang convergence if it were) still has its findings reported instead of
	// dropped. It always includes the required bots: a bot crq waits for whose
	// findings it didn't surface would hang the loop forever.
	extractBots := cfg.evidenceBots()

	// Review-body findings — CodeRabbit's detailed and "Prompt for AI agents"
	// blocks — carry no per-finding resolution state, only the review's commit.
	// When inline comments fail to post (GitHub 5xx / code-review limits) that
	// body block is the ONLY record of the findings, so gating extraction to the
	// current head silently drops an entire review the moment the head moves on
	// (a rebase, a squash-merge). Extract instead from each bot's LATEST review
	// regardless of its commit: a newer review from the same bot supersedes it,
	// and resolved/outdated inline threads still suppress individual prompt
	// duplicates below. Convergence (engine.Completion) stays gated to a review
	// whose commit matches the head, so the loop still waits for a real review.
	latestReview := map[string]ghapi.Review{}
	for _, review := range obs.reviews {
		login := review.User.Login
		if !dialect.InBots(extractBots, login) {
			continue
		}
		// Once a fresh review round has started for this head, a body submitted
		// before that round belongs to the previous head. Unresolved threads are
		// still surfaced below across commits, while thread-less body findings
		// must be re-reported by the current round instead of trapping the loop.
		if anchorOK && cfg.isConfiguredBot(login) &&
			(head == "" || !strings.HasPrefix(review.CommitID, head)) &&
			!notBefore(review.SubmittedAt, anchorCutoff) {
			continue
		}
		if cur, ok := latestReview[login]; !ok || reviewNewer(review, cur) {
			latestReview[login] = review
		}
	}
	for _, review := range obs.reviews {
		if lr, ok := latestReview[review.User.Login]; !ok || lr.ID != review.ID {
			continue
		}
		report.Findings = append(report.Findings, parseReviewBodyFindings(review, review.User.Login)...)
	}

	suppressPromptAt := map[string]bool{}
	// Bugbot re-reports one BUGBOT_BUG_ID across several threads. Dedupe alone
	// collapses only what is emitted together, so resolving the emitted thread
	// simply promotes a sibling on the next poll and the "single" finding
	// resurfaces until every duplicate is resolved by hand. Collect the stable
	// ids settled in ANY thread first and suppress the whole family.
	settledStableIDs := map[string]bool{}
	threadRead := <-threadsCh
	if threads, err := threadRead.threads, threadRead.err; err == nil {
		// A stable id can appear in both settled and open threads, and the two
		// readings pull opposite ways: an open thread may be a genuine re-report
		// after a regression, or merely a leftover duplicate of the one just
		// resolved. A blanket "open wins" resurrected settled findings whenever a
		// duplicate sibling lingered; a blanket "settled wins" buried real
		// re-reports. Comment timestamps cannot separate them either — resolving
		// a thread does not restamp its comment, so the settled occurrence stays
		// older than a sibling reported minutes before it and every duplicate
		// reads as a regression.
		//
		// The commit each occurrence names is what actually distinguishes them:
		// Bugbot stamps every finding with the SHA it reviewed. A regression is
		// re-reported ON THE CURRENT HEAD; a leftover duplicate names an older
		// one. So the family is settled unless it is open at the head and NOT
		// settled at the head — the second half is what keeps three same-head
		// siblings from resurrecting each other as one is resolved.
		openAtHead := map[string]bool{}
		settledAtHead := map[string]bool{}
		anySettled := map[string]bool{}
		for _, thread := range threads {
			settled := thread.IsResolved || thread.IsOutdated
			for _, c := range thread.Comments.Nodes {
				co, ok := dialect.CoReviewerByName(c.Author.Login)
				if !ok || co.FindingDedupeKey == nil {
					continue
				}
				stable, ok := co.FindingDedupeKey(c.Body)
				if !ok {
					continue
				}
				key := dialect.NormalizeBotName(c.Author.Login) + "|" + stable
				atHead := false
				if co.ReviewedCommitSHA != nil && head != "" {
					atHead = dialect.SHAPrefixMatch(co.ReviewedCommitSHA(c.Body), head)
				}
				switch {
				case settled:
					anySettled[key] = true
					settledAtHead[key] = settledAtHead[key] || atHead
				default:
					openAtHead[key] = openAtHead[key] || atHead
				}
			}
		}
		for key := range anySettled {
			if !openAtHead[key] || settledAtHead[key] {
				settledStableIDs[key] = true
			}
		}
		for _, thread := range threads {
			report.Findings = append(report.Findings, threadFindings(thread, extractBots)...)
			// A resolved/outdated inline thread emits no finding, but CodeRabbit's
			// "Prompt for AI agents" block still lists the same location. Record it so
			// the prompt duplicate is suppressed too — otherwise an addressed finding
			// reappears as a thread-less prompt finding and the loop never converges.
			if thread.IsResolved || thread.IsOutdated {
				for _, key := range promptSuppressKeys(thread, extractBots) {
					suppressPromptAt[key] = true
				}
				// A resolved thread where the bot got the last word contesting the
				// agent's decline is NOT actually settled — surface the rebuttal so
				// the loop re-addresses it instead of silently dropping it.
				if rebuttal := threadRebuttal(thread, extractBots); rebuttal != nil {
					report.Findings = append(report.Findings, *rebuttal)
				}
			}
		}
	} else if ghapi.IsThrottled(err) {
		// A transient GraphQL throttle must not silently degrade to the REST
		// fallback, which loses thread resolution/outdated state and the cross-commit
		// unresolved findings this command promises. Surface it so Loop rides it out
		// instead of reporting converged from incomplete data.
		return report, err
	} else {
		comments, cerr := s.gh.ListReviewComments(ctx, repo, pr)
		if cerr != nil {
			return report, cerr
		}
		for _, comment := range comments {
			if !dialect.InBots(extractBots, comment.User.Login) {
				continue
			}
			// The GraphQL path drops findings the bot settled by EDITING its own
			// comment (Macroscope's "✅ Resolved in <sha>"). This fallback must
			// apply the same registry hook, or under it the resolved finding
			// stays actionable indefinitely and convergence never comes.
			if co, ok := dialect.CoReviewerByName(comment.User.Login); ok &&
				co.ResolvedInSHA != nil && co.ResolvedInSHA(comment.Body) != "" {
				continue
			}
			commit := dialect.ShortOID(firstNonEmpty(comment.CommitID, comment.OriginalCommitID))
			if head != "" && commit != "" && commit != head {
				continue
			}
			report.Findings = append(report.Findings, dialect.Finding{
				Bot:       comment.User.Login,
				Severity:  dialect.SeverityFor(comment.User.Login, comment.Body),
				Path:      comment.Path,
				Line:      firstPositive(comment.Line, comment.OriginalLine),
				Title:     dialect.ReviewTitleFor(comment.User.Login, comment.Body),
				Body:      strings.TrimSpace(comment.Body),
				CommentID: comment.ID,
				ReviewID:  comment.PullRequestReviewID,
				Commit:    commit,
				URL:       comment.URL,
				Source:    "review_comment",
				CreatedAt: comment.CreatedAt,
			})
		}
	}

	// Top-level issue comments carry no commit SHA, so bound them to the current
	// head: a bot finding posted before this head was committed belongs to an earlier
	// round and must not trap crq loop on stale, already-addressed feedback. The
	// head commit time is resolved lazily — only when there's an actionable candidate.
	headCutoff := time.Time{}
	headCutoffLoaded := false
	headCutoffOf := func() time.Time {
		// observe() is the single place that asks GitHub what happened on this
		// PR, and it already resolved the head commit date; only fall back to a
		// second fetch when it could not.
		if !obs.eng.HeadAt.IsZero() {
			return obs.eng.HeadAt
		}
		if !headCutoffLoaded {
			headCutoffLoaded = true
			if pull.Head.SHA != "" {
				if c, cerr := s.gh.GetCommit(ctx, repo, pull.Head.SHA); cerr == nil {
					headCutoff = c.Committer.Date
				}
			}
		}
		return headCutoff
	}
	// Classified co-reviewer events (clean summaries, verdicts, notices,
	// trigger commands, unable notices) are completion/participation signals,
	// not actionable findings — index them by comment id so the loop below
	// skips them (the same pattern as the Codex clean-summary skip, which the
	// classifier now owns generically).
	coEventKinds := map[int64]dialect.EventKind{}
	for _, ev := range obs.eng.Events {
		switch ev.Kind {
		case dialect.EvCoClean, dialect.EvCoUnable, dialect.EvCoNotice, dialect.EvCoVerdict, dialect.EvCoCommand:
			coEventKinds[ev.CommentID] = ev.Kind
		}
	}
	for _, comment := range obs.comments {
		if !dialect.InBots(extractBots, comment.User.Login) {
			continue
		}
		if s.cr.IsReviewSkipped(comment.Body) && cfg.isConfiguredBot(comment.User.Login) &&
			skipAppliesToHead(comment.Body, head) &&
			!skipPredatesHead(comment, headCutoffOf) {
			// Checked BEFORE the rate-limit guard below: the skip notice embeds
			// the rate-limit marker, so that guard would drop it and the round
			// would converge with a silently absent primary reviewer — worse than
			// a loud one. The review did not happen and never will for this head,
			// so surface it as work: narrow the PR to get a review.
			// The reviewer's own name, not a hardcoded one: the primary bot is
			// configurable (CRQ_BOT), so naming CodeRabbit here mislabels the
			// notice on every repo pointed at a different reviewer.
			who := dialect.NormalizeBotName(comment.User.Login)
			reason := dialect.ReviewSkippedReason(comment.Body)
			if reason == "" {
				reason = who + " skipped the review for this head."
			}
			report.Findings = append(report.Findings, dialect.Finding{
				Bot: comment.User.Login,
				// Bound to the head so Loop's pre-enqueue blocking check treats
				// it as work for THIS head rather than a reason never to start:
				// returning exit 10 here short-circuited waitToFire, so the
				// co-reviewers that are supposed to resolve a skipped round
				// never ran at all.
				Commit:    head,
				Severity:  "major",
				Title:     who + " skipped this review — narrow the PR to get one: " + reason,
				Body:      strings.TrimSpace(dialect.CompactReviewBody(comment.Body)),
				CommentID: comment.ID,
				URL:       comment.URL,
				Source:    skipNoticeSource,
				CreatedAt: comment.CreatedAt,
			})
			continue
		}
		if s.cr.IsRateLimited(comment.Body) {
			continue // an account-quota notice is never a finding
		}
		// Co-reviewer clean summaries/verdicts/notices and every configured-bot
		// issue comment are completion signals, not actionable findings:
		// engine.Completion already folded them into ReviewedBy, so they are
		// never surfaced here.
		if _, ok := coEventKinds[comment.ID]; ok {
			continue
		}
		if cfg.isConfiguredBot(comment.User.Login) {
			continue
		}
		if dialect.IsNonActionableText(comment.Body) {
			continue // notices/acks (e.g. usage-limit messages) aren't findings
		}
		if cutoff := headCutoffOf(); !cutoff.IsZero() && comment.CreatedAt.Before(cutoff) {
			continue // posted before the current head was committed — a stale round
		}
		report.Findings = append(report.Findings, dialect.Finding{
			Bot:       comment.User.Login,
			Severity:  dialect.SeverityFor(comment.User.Login, comment.Body),
			Title:     dialect.ReviewTitleFor(comment.User.Login, comment.Body),
			Body:      strings.TrimSpace(comment.Body),
			CommentID: comment.ID,
			URL:       comment.URL,
			Source:    "issue_comment",
			CreatedAt: comment.CreatedAt,
		})
	}

	report.Findings = dedupeFindings(report.Findings, suppressPromptAt, settledStableIDs)
	// Dismissals apply HERE, not in the caller: convergence and `crq loop`'s exit
	// code are computed from this list, so filtering further out would leave a
	// dismissed finding permanently actionable everywhere except `crq next`.
	if round != nil && round.Head == head && len(round.Dismissed) > 0 {
		kept := make([]dialect.Finding, 0, len(report.Findings))
		for _, finding := range report.Findings {
			// Only where the source itself cannot carry a thread. IDs hash the
			// text, not the source, so a body finding later delivered as an inline
			// comment through the REST fallback hashes the same — and filtering on
			// the ID alone would hide a review thread that is open.
			if dismissibleSources[finding.Source] && finding.ThreadID == "" && round.IsDismissed(finding.ID) {
				continue
			}
			kept = append(kept, finding)
		}
		report.Findings = kept
		// The count is of the DECISIONS this round recorded, not of the findings
		// that happened to still match one. A bot that edits or deletes the
		// comment a dismissal was made against leaves nothing to match, and
		// reporting zero there would make a set-aside finding indistinguishable
		// from one that was never reported at all.
		report.Dismissed = len(round.Dismissed)
	}
	sort.Slice(report.Findings, func(i, j int) bool {
		if dialect.RankSeverity(report.Findings[i].Severity) != dialect.RankSeverity(report.Findings[j].Severity) {
			return dialect.RankSeverity(report.Findings[i].Severity) > dialect.RankSeverity(report.Findings[j].Severity)
		}
		if report.Findings[i].Path != report.Findings[j].Path {
			return report.Findings[i].Path < report.Findings[j].Path
		}
		return report.Findings[i].Line < report.Findings[j].Line
	})
	report.Converged = engine.Converged(report.Findings, completion)
	// Degrade detection: a live rate-limit window plus observed Codex
	// responsiveness means this round runs Codex-only for now. Converged is
	// structurally false here (CodeRabbit has no review evidence), so a
	// deferred round can never masquerade as converged. Only the ACCOUNT-wide
	// quota block qualifies — a round's own awaiting_retry cooldown also
	// covers non-quota retries (post failures, timeouts) that must keep their
	// normal retry handling.
	// Gated on Codex being a reviewer HERE. The degrade releases the head on
	// Codex's word while the primary's window is shut; on a repository that
	// excluded Codex, an unsolicited auto-review would otherwise stand in for a
	// bot nobody asked for.
	if cfg.RateLimitCoDegrade && cfg.coBotEnabled(dialect.CodexBotLogin) && !report.Converged && st.Account.BlockedUntil != nil {
		until := st.Account.BlockedUntil.UTC()
		if until.After(now) && engine.CodexOnlyEligible(completionRound, obs.eng, &until, now) {
			report.CodeRabbitDeferred = true
			report.DeferredUntil = &until
		}
	}
	switch {
	case report.Converged:
		report.Status = "converged"
	case report.CodeRabbitDeferred && len(report.Findings) == 0 &&
		engine.DoneExceptWithEvidence(report.ReviewedBy, cfg.Bot, dialect.CodexBotLogin):
		report.Status = "deferred"
		report.Reason = "codex reviewed clean; coderabbit review deferred until " +
			report.DeferredUntil.UTC().Format(time.RFC3339) + " (account rate-limited)"
	case len(report.Findings) == 0:
		report.Status = "waiting"
	}
	return report, nil
}

func (s *Service) loopClaimed(ctx context.Context, repo string, pr int) (FeedbackReport, int, error) {
	repo = NormalizeRepo(repo)
	// Do not spend a new review slot while actionable feedback from an earlier
	// round is still open. Feedback intentionally carries unresolved threads
	// across commits; surfacing them here makes "fix, resolve, then re-review" a
	// hard loop invariant instead of something every agent has to remember.
	//
	// An active wait for this head is different: extraction-only bots can answer
	// before the required reviewer, and those findings must remain buffered until
	// the configured reviewer gate completes.
	head, open, err := s.pullHead(ctx, repo, pr)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	if open {
		state, _, loadErr := s.store.Load(ctx)
		if loadErr != nil {
			return FeedbackReport{}, 1, loadErr
		}
		if state.WaitingHead(repo, pr) != head {
			for {
				report, feedbackErr := s.Feedback(ctx, repo, pr)
				if feedbackErr != nil {
					if wait, ok := ghapi.ThrottleWait(feedbackErr); ok {
						if wait <= 0 {
							wait = s.cfg.PollInterval
						}
						if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
							return report, 1, serr
						}
						continue
					}
					return report, 1, feedbackErr
				}
				// The guard exists to stop a NEW review round starting while
				// earlier findings are open. The skip notice is the one finding
				// that must not hold it: it has no thread to resolve, so
				// returning here short-circuited waitToFire forever and the
				// co-reviewers meant to resolve a skipped round never ran.
				// Exempt exactly that notice — real unresolved findings on a
				// summary-only PR still hold the head, as on any other.
				blocking := excludeSkipNotice(engine.BlockingFindings(report.Findings, head))
				if len(blocking) > 0 {
					report.Findings = blocking
					report.Status = "feedback"
					report.Reason = "unresolved findings must be addressed before a new review round"
					return report, 10, nil
				}
				break
			}
		}
	}
	waitResult, waitCode, err := s.waitToFire(ctx, repo, pr)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	// A hold ends the loop wherever it is reported. It is administrative and
	// indefinite for this run: entering the feedback poll would wait out a whole
	// second timeout for a review that will not be requested until a person lifts
	// it or the daemon observes that the pull request merged.
	//
	// Code 0, not 2. The exit codes are frozen with 2 meaning an ELAPSED wait,
	// so returning it for a hold tells a caller scripted against that contract
	// to retry something only an unhold or merge cleanup ends. Terminal like
	// "skipped"; the status is what tells them which.
	if waitResult.Action == "held" {
		return FeedbackReport{
			Status: "held", Repo: NormalizeRepo(repo), PR: pr,
			Head: waitResult.Head, Reason: waitResult.Reason,
			ReviewedBy: map[string]bool{}, Findings: []dialect.Finding{},
		}, 0, nil
	}
	if waitCode == 2 {
		status := "timeout"
		code := 2
		switch waitResult.Action {
		case "skipped":
			status = "skipped"
			code = 0
			s.completeWaitRound(ctx, repo, pr, "", false, nil)
		}
		// The slot wait timed out (CRQ_WAIT_TIMEOUT) without firing a review. Don't
		// enter the feedback poll — that would burn another feedback timeout and could
		// return stale pre-existing findings despite no new review round. Report the
		// timeout so the caller retries later instead. A skipped wait result is
		// terminal, not retryable, so preserve it as a skipped report.
		if waitResult.Action == "skipped" {
			s.completeWaitRound(ctx, repo, pr, "", false, nil)
		}

		// return stale pre-existing findings despite no new review round. A held
		// result is administrative, not completion: an already-fired round must
		// stay open so its acknowledgement still owns the FireSlot.
		return FeedbackReport{Status: status, Repo: NormalizeRepo(repo), PR: pr, Head: waitResult.Head, Reason: waitResult.Reason, ReviewedBy: map[string]bool{}, Findings: []dialect.Finding{}}, code, nil
	}
	head = waitResult.Head
	if head == "" {
		var herr error
		head, _, herr = s.pullHead(ctx, repo, pr)
		if herr != nil {
			return FeedbackReport{}, 1, herr
		}
	}
	deadline, err := s.ensureWaitDeadline(ctx, repo, pr, head)
	if err != nil {
		return FeedbackReport{}, 1, err
	}
	var lastLog time.Time
	var settledAt time.Time
	// Pump keeps the queue moving while we wait, but once a minute is plenty (the
	// autoreview daemon pumps too); pumping on every tick just burns REST quota.
	var lastPump time.Time
	pumpEvery := pumpEveryFor(s.cfg.PollInterval)
	for {
		report, err := s.Feedback(ctx, repo, pr)
		if err != nil {
			// A GitHub REST throttle (the shared 5000/hr quota) is transient — ride
			// it out like a network outage rather than failing the agent. Wait for the
			// reset and push the review deadline past it: GitHub throttling isn't the
			// bot taking long to review.
			if wait, ok := ghapi.ThrottleWait(err); ok {
				if wait <= 0 {
					wait = s.cfg.PollInterval
				}
				deadline = deadline.Add(wait)
				if s.log != nil {
					s.log.Printf("%s#%d GitHub API throttled; waiting %s for the reset, then resuming", repo, pr, wait.Round(time.Second))
				}
				s.pushWaitDeadline(ctx, repo, pr, head, deadline)
				if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
					return report, 1, serr
				}
				continue
			}
			return report, 1, err
		}
		// Findings are work immediately, even when another required reviewer is
		// still pending. Return control so the caller can fix locally, but keep the
		// reviewed head unchanged until every required bot finishes; pushing early
		// restarts the remaining checks and wastes the review slot.
		if len(report.Findings) > 0 {
			report.Status = "feedback"
			if allReviewed(report.ReviewedBy) {
				report.Reason = "all required reviewers finished; address findings, push once, and resolve threads"
				s.completeWaitRound(ctx, repo, pr, head, report.PrimaryAckPending, &report.config)
			} else if report.CodeRabbitDeferred && engine.DoneExceptWithEvidence(report.ReviewedBy, report.config.Bot, dialect.CodexBotLogin) {
				// Degraded round: every required bot except the rate-limited
				// CodeRabbit has finished. These findings are this round's work —
				// fixing and pushing is exactly right; the CodeRabbit review stays
				// queued and fires against the newest head once the window opens.
				// With ANOTHER required bot still pending, the hold-head branch
				// below applies instead: pushing would restart its checks.
				report.Reason = "codex findings during a coderabbit rate-limit window; fix, push, and loop again — the coderabbit review stays queued and fires when the window opens"
			} else if report.PrimaryUnavailable {
				// Only co-reviewers are outstanding: naming them (and the fact that
				// the primary is out of the picture) stops the agent inferring it
				// must wait on the primary or on an account-quota window.
				report.Reason = "hold current head for the remaining co-reviewers only — " +
					report.PrimaryUnavailableReason + ", so nothing here waits on it or on the account quota"
			} else {
				// A required reviewer is still pending (e.g. Codex posted a finding
				// before CodeRabbit reviewed). Return the findings to work on, but leave
				// the round active — completing it would release the slot and stop
				// observing the pending bot's review/rate-limit/timeout, losing evidence
				// the loop is still obligated to wait for.
				report.Reason = "hold current head: fix locally, but do not commit or push until every required reviewer finishes"
			}
			return report, 10, nil
		}
		if report.Converged || report.Status == "deferred" {
			// Don't trust the first converged observation: bots deliver in waves
			// (Codex auto-reviews a pushed head minutes later; CodeRabbit's real
			// review can trail its comment shells). Hold the verdict for the settle
			// window and only exit 0 if nothing new lands; any finding or pending
			// reviewer resets the normal flow above. A deferred (Codex-only) clean
			// verdict settles the same way but must NOT complete the round — the
			// queued CodeRabbit review is still owed for this head.
			if settledAt.IsZero() {
				settledAt = s.clock()
			}
			// report.config, not s.cfg: every setting the wait is paced by is
			// fleet-settable, and this loop re-resolves it against the state ref
			// on each poll. Reading the startup env would keep an in-flight wait
			// running on a cadence the dashboard has already replaced.
			if report.config.SettleWindow <= 0 || s.clock().Sub(settledAt) >= report.config.SettleWindow {
				if report.Converged {
					s.completeWaitRound(ctx, repo, pr, head, report.PrimaryAckPending, &report.config)
				}
				return report, 0, nil
			}
		} else {
			settledAt = time.Time{}
		}
		// Keep the queue moving (re-fire once an account-block window clears) and pick up
		// the Blocked state it leaves behind. Pumping every poll tick is redundant —
		// with several loops waiting concurrently it multiplies into real REST-quota
		// cost — so each waiter pumps at most once per pumpEvery.
		if lastPump.IsZero() || time.Since(lastPump) >= pumpEvery {
			if _, err := s.Pump(ctx); err != nil && s.log != nil {
				s.log.Printf("warning: pump while waiting for feedback failed: %v", err)
			}
			lastPump = time.Now()
		}
		// While the account is blocked the PR can't be reviewed yet — it just
		// stays queued — so that wait must not count against the feedback timeout, and
		// there's nothing to fetch until the window clears. Push the deadline past the
		// block and poll slowly, so a long queue wait doesn't drain the shared GitHub
		// REST quota (and re-hit its throttle) every PollInterval.
		poll := s.cfg.PollInterval
		var blockedUntil *time.Time
		now := s.clock()
		// A round the primary will never review neither spends the account
		// quota nor waits on it. Consulting the block would extend this round's
		// deadline, slow its poll to the block window, and narrate a limit that
		// has nothing to do with it — while the co-reviewers it IS waiting for
		// answer in minutes. Leave blockedUntil nil so none of that applies.
		if !report.PrimaryUnavailable {
			if st, _, lerr := s.store.Load(ctx); lerr == nil {
				if until, ok := st.AccountBlockedUntil(repo, pr, head, now); ok {
					blockedUntil = &until
				}
			}
		}
		// A degraded round waits for Codex, not for the window: keep the normal
		// poll cadence against the un-extended deadline. (The rate-limit reply
		// usually lands before Codex answers, so an iteration or two may extend
		// the deadline first — harmless: once Codex evidence arrives the loop
		// exits promptly, and if Codex never answers the round degrades
		// gracefully back to riding out the window.)
		if blockedUntil != nil && !report.CodeRabbitDeferred {
			extended := extendDeadlineForBlock(deadline, blockedUntil, now, report.config.FeedbackWaitTimeout)
			if extended.After(deadline) {
				deadline = extended
				s.pushWaitDeadline(ctx, repo, pr, head, deadline)
			}
			poll = blockedPollInterval(*blockedUntil, now, s.cfg.PollInterval)
		}
		if now.After(deadline) && settledAt.IsZero() {
			// A degraded round must not be completed on timeout: marking the head
			// reviewed would silently cancel the still-owed CodeRabbit review.
			if !report.CodeRabbitDeferred {
				s.completeWaitRound(ctx, repo, pr, head, false, &report.config)
			}
			if len(report.Findings) > 0 {
				report.Status = "feedback"
				report.Reason = "review wait timed out; actionable findings must be addressed before retrying"
				return report, 10, nil
			}
			report.Status = "timeout"
			return report, 2, nil
		}
		if s.log != nil && time.Since(lastLog) >= 30*time.Second {
			activeElapsed := feedbackWaitElapsed(deadline, report.config.FeedbackWaitTimeout, now)
			if blockedUntil != nil && report.CodeRabbitDeferred {
				s.log.Printf("%s#%d degraded to codex-only — coderabbit rate-limited until %s; waiting for codex on %s (%s / %s)", repo, pr, blockedUntil.UTC().Format(time.RFC3339), report.Head, activeElapsed.Round(time.Second), report.config.FeedbackWaitTimeout)
			} else if blockedUntil != nil {
				s.log.Printf("%s#%d queued — account blocked until %s; waiting, not counting it against the %s review wait (%s active)", repo, pr, blockedUntil.UTC().Format(time.RFC3339), report.config.FeedbackWaitTimeout, activeElapsed.Round(time.Second))
			} else {
				s.log.Printf("%s#%d waiting for review feedback on %s — reviewed %s (%s / %s)", repo, pr, report.Head, reviewedSummary(report.ReviewedBy), activeElapsed.Round(time.Second), report.config.FeedbackWaitTimeout)
			}
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return report, 1, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ensureWaitDeadline returns the wall-clock deadline the loop bounds its poll
// by. The wait IS the round: when the fired/reviewing round has no WaitDeadline
// yet (Pump normally sets it at fire time), this sets one budget past the fire.
// If the round is no longer waiting (completed/none), it returns a transient
// deadline so the loop still terminates.
func (s *Service) ensureWaitDeadline(ctx context.Context, repo string, pr int, head string) (time.Time, error) {
	repo = NormalizeRepo(repo)
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if dl, ok := st.RoundWaitDeadline(repo, pr, head); ok {
		return dl, nil
	}
	changed := false
	updated, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(repo, pr)
		if r == nil || r.Head != head || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) || r.WaitDeadline != nil {
			return ErrNoChange
		}
		start := s.clock()
		if r.FiredAt != nil {
			start = r.FiredAt.UTC()
		}
		dl := start.Add(s.feedbackWait(*st))
		r.WaitDeadline = &dl
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	if changed {
		s.sync(ctx, updated)
	}
	if dl, ok := updated.RoundWaitDeadline(repo, pr, head); ok {
		return dl, nil
	}
	// The round is no longer a wait (completed/none): synthesize a transient
	// deadline so the loop still bounds its poll.
	return s.clock().Add(s.feedbackWait(updated)), nil
}

// pushWaitDeadline moves the fired/reviewing round's wait deadline later (never
// earlier), persisting the extension an account block or GitHub throttle bought.
func (s *Service) pushWaitDeadline(ctx context.Context, repo string, pr int, head string, deadline time.Time) {
	repo = NormalizeRepo(repo)
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(repo, pr)
		if r == nil || r.Head != head || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
			return ErrNoChange
		}
		if r.WaitDeadline != nil && !deadline.After(*r.WaitDeadline) {
			return ErrNoChange
		}
		dl := deadline.UTC()
		r.WaitDeadline = &dl
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: failed to persist feedback wait deadline for %s#%d: %v", repo, pr, err)
		}
		return
	}
	if changed {
		s.sync(ctx, state)
	}
}

// completeWaitRound ends the wait by completing the fired/reviewing round. The
// completed round remains as the "this head was reviewed" dedup marker, so a
// subsequent enqueue/needsReview at the same head is deduped rather than re-fired.
//
// holdUnacked leaves alone a round still holding the fire slot for a review
// command the primary has not acknowledged: with the primary outside the
// required set the round converges without it, and completing here would release
// the slot to the next pull request while this one's metered command is
// unanswered — the same release Progress now withholds. The round stays fired
// and any Pump finishes it, on the acknowledgement or on the in-flight timeout,
// and the hold is stamped on the slot so the push this success invites cannot
// drop it along with the superseded round. Only the convergence callers pass it;
// a timed-out or never fired wait must still end.
func (s *Service) completeWaitRound(ctx context.Context, repo string, pr int, head string, holdUnacked bool, cfg *Config) error {
	repo = NormalizeRepo(repo)
	changed := false
	state, err := s.store.Update(ctx, func(st *State) error {
		changed = false
		r := st.Round(repo, pr)
		if r == nil || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
			return ErrNoChange
		}
		if head != "" && r.Head != head {
			return ErrNoChange
		}
		if cfg != nil && reviewersChanged(st, repo, *cfg) {
			return ErrNoChange
		}
		if holdUnacked && st.FireSlot != nil && st.FireSlot.Key == QueueKey(repo, pr) {
			// Leaving the round fired keeps the slot only while the round exists,
			// and converging is precisely the loop's signal to push: the head
			// advance that follows archives this round, and the slot would be
			// released with the command it was taken for still unanswered. So
			// record the hold on the slot itself, where it survives the supersede.
			// Bounded by the in-flight window, the deadline Progress gives up at
			// — and read from the SAME resolved configuration Progress uses, not
			// from the env this process started with. A fleet that lengthened the
			// timeout would otherwise have the slot released here while the
			// command was still in flight, and one that shortened it would hold
			// the fleet for a window nothing is waiting out.
			if r.FiredAt != nil {
				inflight := s.cfg.InflightTimeout
				if cfg != nil {
					inflight = cfg.InflightTimeout
				}
				until := r.FiredAt.UTC().Add(inflight)
				if until.After(s.clock()) && (st.FireSlot.HoldUntil == nil || st.FireSlot.HoldUntil.Before(until)) {
					st.HoldSlotUntil(until)
					changed = true
					return nil
				}
			}
			return ErrNoChange
		}
		ownerToken := r.Token
		if err := r.Complete(); err != nil {
			return err
		}
		releaseSlot(st, QueueKey(repo, pr), ownerToken)
		st.PutRound(*r)
		changed = true
		return nil
	})
	if err != nil {
		if s.log != nil {
			s.log.Printf("warning: failed to clear feedback wait for %s#%d: %v", repo, pr, err)
		}
		return err
	}
	if changed {
		s.sync(ctx, state)
	}
	return nil
}

// waitToFire runs Wait (enqueue + coordinated fire), riding out GitHub REST rate
// limits the same way the feedback loop does instead of failing the agent on a
// transient throttle. Returns Wait's result and exit code (3 = already reviewed
// for head).
func (s *Service) waitToFire(ctx context.Context, repo string, pr int) (PumpResult, int, error) {
	for {
		result, code, err := s.Wait(ctx, repo, pr)
		if err == nil {
			return result, code, nil
		}
		wait, ok := ghapi.ThrottleWait(err)
		if !ok {
			return result, code, err
		}
		if wait <= 0 {
			wait = s.cfg.PollInterval
		}
		if s.log != nil {
			s.log.Printf("%s#%d GitHub API throttled before firing; waiting %s for the reset, then retrying", repo, pr, wait.Round(time.Second))
		}
		if serr := ghapi.SleepCtx(ctx, wait); serr != nil {
			return result, code, serr
		}
	}
}

// blockedPollInterval slows the feedback poll while the account is blocked:
// nothing can be fetched until the window clears, so wait until just past the
// reset instead of every PollInterval — capped so the loop still re-checks
// periodically. Keeps a long queue wait from draining the shared GitHub REST quota.
// pumpEveryFor bounds how often a waiting feedback loop pumps the queue: never
// more than once a minute, and never more often than it polls. Several loops
// waiting concurrently each used to pump every tick, which multiplied into real
// REST-quota drain for zero extra queue throughput.
func pumpEveryFor(poll time.Duration) time.Duration {
	if poll < time.Minute {
		return time.Minute
	}
	return poll
}

func blockedPollInterval(blockedUntil, now time.Time, base time.Duration) time.Duration {
	const maxWait = 5 * time.Minute
	wait := blockedUntil.Sub(now) + time.Second
	if wait < base {
		return base
	}
	if wait > maxWait {
		return maxWait
	}
	return wait
}

// extendDeadlineForBlock keeps the feedback-wait deadline from elapsing while the
// CodeRabbit account is blocked. A blocked PR can't be reviewed — it just
// stays queued until the window clears and crq re-fires — so that time shouldn't
// burn the review-wait budget. When blocked, the deadline is pushed to a full
// budget past the block; it is never moved earlier.
func extendDeadlineForBlock(deadline time.Time, blockedUntil *time.Time, now time.Time, budget time.Duration) time.Time {
	if blockedUntil == nil || !blockedUntil.After(now) {
		return deadline
	}
	if extended := blockedUntil.Add(budget); extended.After(deadline) {
		return extended
	}
	return deadline
}

// feedbackWaitElapsed reports only reviewable time. An account block extends
// deadline, so deriving progress from the remaining budget excludes the blocked
// interval and can never produce impossible output such as "35m / 20m".
func feedbackWaitElapsed(deadline time.Time, budget time.Duration, now time.Time) time.Duration {
	if budget <= 0 {
		return 0
	}
	elapsed := budget - deadline.Sub(now)
	if elapsed < 0 {
		return 0
	}
	if elapsed > budget {
		return budget
	}
	return elapsed
}

func reviewedSummary(m map[string]bool) string {
	if len(m) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		state := "waiting"
		if m[k] {
			state = "done"
		}
		parts = append(parts, k+"="+state)
	}
	return strings.Join(parts, " ")
}

type ResolvedThread struct {
	ThreadID string `json:"thread_id"`
	Resolved bool   `json:"resolved"`
}

func (s *Service) ResolveThreads(ctx context.Context, threadIDs []string) ([]ResolvedThread, error) {
	var out []ResolvedThread
	for _, id := range threadIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var result struct {
			ResolveReviewThread struct {
				Thread struct {
					ID         string `json:"id"`
					IsResolved bool   `json:"isResolved"`
				} `json:"thread"`
			} `json:"resolveReviewThread"`
		}
		err := s.gh.GraphQL(ctx, `mutation($id:ID!){
  resolveReviewThread(input:{threadId:$id}) {
    thread { id isResolved }
  }
}`, map[string]any{"id": id}, &result)
		if err != nil {
			return out, err
		}
		out = append(out, ResolvedThread{ThreadID: result.ResolveReviewThread.Thread.ID, Resolved: result.ResolveReviewThread.Thread.IsResolved})
	}
	return out, nil
}

type DeclinedThread struct {
	ThreadID string `json:"thread_id"`
	URL      string `json:"url,omitempty"`
	Resolved bool   `json:"resolved"`
}

// DeclineThreads posts a reason as a reply on each review thread, documenting why
// a finding is not being addressed. By default the thread is left unresolved (an
// on-the-record disagreement); pass resolve=true to also close it ("won't fix").
func (s *Service) DeclineThreads(ctx context.Context, threadIDs []string, reason string, resolve bool) ([]DeclinedThread, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("a decline reason is required (--reason)")
	}
	var out []DeclinedThread
	for _, id := range threadIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var reply struct {
			AddPullRequestReviewThreadReply struct {
				Comment struct {
					URL string `json:"url"`
				} `json:"comment"`
			} `json:"addPullRequestReviewThreadReply"`
		}
		err := s.gh.GraphQL(ctx, `mutation($threadId:ID!,$body:String!){
  addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}) {
    comment { url }
  }
}`, map[string]any{"threadId": id, "body": reason}, &reply)
		if err != nil {
			return out, err
		}
		dt := DeclinedThread{ThreadID: id, URL: reply.AddPullRequestReviewThreadReply.Comment.URL}
		if resolve {
			resolved, rerr := s.ResolveThreads(ctx, []string{id})
			if rerr != nil {
				return out, rerr
			}
			if len(resolved) > 0 {
				dt.Resolved = resolved[0].Resolved
			}
		}
		out = append(out, dt)
	}
	return out, nil
}

type reviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Comments   struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			DatabaseID   int64     `json:"databaseId"`
			Body         string    `json:"body"`
			URL          string    `json:"url"`
			Path         string    `json:"path"`
			Line         int       `json:"line"`
			OriginalLine int       `json:"originalLine"`
			CreatedAt    time.Time `json:"createdAt"`
			Author       struct {
				Login string `json:"login"`
			} `json:"author"`
			Commit struct {
				OID string `json:"oid"`
			} `json:"commit"`
			OriginalCommit struct {
				OID string `json:"oid"`
			} `json:"originalCommit"`
		} `json:"nodes"`
	} `json:"comments"`
}

func (s *Service) reviewThreads(ctx context.Context, repo string, pr int) ([]reviewThread, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var all []reviewThread
	cursor := ""
	for {
		var result struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []reviewThread `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		variables := map[string]any{"owner": owner, "name": name, "number": pr, "cursor": nil}
		if cursor != "" {
			variables["cursor"] = cursor
		}
		query := `query($owner:String!, $name:String!, $number:Int!, $cursor:String) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      reviewThreads(first:100, after:$cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated path line
          comments(first:50) {
            totalCount
            nodes {
              databaseId body url path line originalLine createdAt
              author { login }
              commit { oid }
              originalCommit { oid }
            }
          }
        }
      }
    }
  }
}`
		if err := s.gh.GraphQL(ctx, query, variables, &result); err != nil {
			return all, err
		}
		page := result.Repository.PullRequest.ReviewThreads
		all = append(all, page.Nodes...)
		if !page.PageInfo.HasNextPage {
			break
		}
		cursor = page.PageInfo.EndCursor
	}
	return all, nil
}

// parseReviewBodyFindings adapts a GitHub review to the dialect parsers, which
// take only the metadata they attach to findings.
func parseReviewBodyFindings(review ghapi.Review, bot string) []dialect.Finding {
	return dialect.ParseReviewBodyFindings(review.Body, dialect.ReviewMeta{
		ID:          review.ID,
		CommitID:    review.CommitID,
		HTMLURL:     review.HTMLURL,
		SubmittedAt: review.SubmittedAt,
	}, bot)
}

// reviewNewer reports whether review a supersedes b: later submission wins, and
// a higher ID breaks ties (equal/zero timestamps) so selection is deterministic.
func reviewNewer(a, b ghapi.Review) bool {
	if !a.SubmittedAt.Equal(b.SubmittedAt) {
		return a.SubmittedAt.After(b.SubmittedAt)
	}
	return a.ID > b.ID
}

// threadRebuttal surfaces a bot's contested reply on a RESOLVED thread as a
// finding. When the agent declines a finding with `crq decline --resolve`, the
// bot often replies conceding ("I'm withdrawing this finding") or contesting
// ("I'm retaining the finding: ..."). threadFindings drops resolved threads, so
// a contest would vanish and the loop would converge over an unaddressed
// rebuttal. This re-surfaces it: the thread's latest comment is a bot reply that
// follows the agent's own comment and does not clearly withdraw the finding.
// Ambiguous replies surface too — never bury a rebuttal on a false concession.
// Returns nil when the thread is unresolved (threadFindings already covers it),
// when the last word is not the bot's, when the agent never replied, or when the
// bot withdrew.
func threadRebuttal(thread reviewThread, bots map[string]struct{}) *dialect.Finding {
	if !thread.IsResolved && !thread.IsOutdated {
		return nil
	}
	nodes := thread.Comments.Nodes
	if len(nodes) < 2 {
		return nil
	}
	// Only judge complete threads: comments(first:50) truncates long
	// discussions, and the "last word" below would be a stale mid-thread reply.
	// Skipping is safe — a thread that long has had human attention.
	if thread.Comments.TotalCount > len(nodes) {
		return nil
	}
	last := nodes[len(nodes)-1]
	if !dialect.InBots(bots, last.Author.Login) {
		return nil // the agent, not the bot, had the last word
	}
	// The rebuttal shape is strictly bot finding → agent reply → bot last word.
	// A human-started thread that a bot merely answered is not a declined
	// finding, and surfacing it would fabricate a contest.
	if !dialect.InBots(bots, nodes[0].Author.Login) {
		return nil
	}
	agentReplied := false
	for _, c := range nodes[1 : len(nodes)-1] {
		if !dialect.InBots(bots, c.Author.Login) {
			agentReplied = true
			break
		}
	}
	if !agentReplied {
		return nil // the bot is talking to itself, not answering a decline
	}
	verdict := dialect.ClassifyDeclineReply(last.Body)
	if verdict == dialect.ReplyWithdrawn {
		return nil // conceded — the decline stands
	}
	if dialect.IsNonActionableText(last.Body) {
		return nil // a platform notice or ack, not a rebuttal (e.g. Codex's
		// "create an environment" boilerplate posted as a thread reply)
	}
	// The verdict decides how this reads. Everything that was not a clear
	// withdrawal used to be announced as a contest, so a bot AGREEING with the
	// decline was reported as standing its ground — and an agent, told to
	// re-address a rebuttal that did not exist, looped on the artifact.
	//
	// Surfacing an unclear reply is still right — a buried rebuttal is the worse
	// failure — and it keeps the same major floor, so visibility is unchanged.
	// Only the CLAIM is corrected, and the wording for it lives with the
	// classifier rather than here.
	severity := dialect.FloorSeverity(dialect.SeverityOf(last.Body), "major")
	title := verdict.TitlePrefix()
	return &dialect.Finding{
		Bot:       last.Author.Login,
		Severity:  severity,
		Path:      firstNonEmpty(thread.Path, last.Path),
		Line:      firstPositive(thread.Line, last.Line, last.OriginalLine),
		Title:     title + dialect.TitleOf(last.Body),
		Body:      strings.TrimSpace(last.Body),
		ThreadID:  thread.ID,
		CommentID: last.DatabaseID,
		URL:       last.URL,
		Source:    "review_reply",
		CreatedAt: last.CreatedAt,
	}
}

// threadFindings turns one GitHub review thread into findings. An unresolved,
// non-outdated thread is still actionable no matter which commit its comments
// were filed on: GitHub's own resolution/outdated state is the source of truth,
// so a real finding from an earlier commit is surfaced instead of silently
// dropped when HEAD moves. (This is why callers do not need a manual
// cross-review audit.) Resolved or outdated threads are skipped.
func threadFindings(thread reviewThread, bots map[string]struct{}) []dialect.Finding {
	if thread.IsResolved || thread.IsOutdated {
		return nil
	}
	var out []dialect.Finding
	for _, comment := range thread.Comments.Nodes {
		if !dialect.InBots(bots, comment.Author.Login) {
			continue
		}
		// A settled-marker edit means the bot considers the finding addressed —
		// Macroscope never resolves threads, it EDITS the comment ("✅ Resolved
		// in <sha>" / "No longer relevant as of <sha>"); the edit IS its
		// resolution, trusted like a thread resolution (no head match required).
		if co, ok := dialect.CoReviewerByName(comment.Author.Login); ok &&
			co.ResolvedInSHA != nil && co.ResolvedInSHA(comment.Body) != "" {
			continue
		}
		commit := dialect.ShortOID(comment.Commit.OID)
		if commit == "" {
			commit = dialect.ShortOID(comment.OriginalCommit.OID)
		}
		out = append(out, dialect.Finding{
			Bot:       comment.Author.Login,
			Severity:  dialect.SeverityFor(comment.Author.Login, comment.Body),
			Path:      firstNonEmpty(thread.Path, comment.Path),
			Line:      firstPositive(thread.Line, comment.Line, comment.OriginalLine),
			Title:     dialect.ReviewTitleFor(comment.Author.Login, comment.Body),
			Body:      strings.TrimSpace(comment.Body),
			ThreadID:  thread.ID,
			CommentID: comment.DatabaseID,
			Commit:    commit,
			URL:       comment.URL,
			Source:    "review_thread",
			CreatedAt: comment.CreatedAt,
		})
	}
	return out
}

// promptSuppressKeys returns the bot|path|line dedupe keys for a thread's bot
// comments, matching the keys dedupeFindings builds for prompt findings, so a
// resolved/outdated thread can suppress its "Prompt for AI agents" duplicate.
func promptSuppressKeys(thread reviewThread, bots map[string]struct{}) []string {
	var keys []string
	for _, comment := range thread.Comments.Nodes {
		if !dialect.InBots(bots, comment.Author.Login) {
			continue
		}
		path := firstNonEmpty(thread.Path, comment.Path)
		line := firstPositive(thread.Line, comment.Line, comment.OriginalLine)
		if path == "" || line <= 0 {
			continue
		}
		keys = append(keys, dialect.NormalizeBotName(comment.Author.Login)+"|"+path+"|"+strconv.Itoa(line))
	}
	return keys
}

func dedupeFindings(in []dialect.Finding, suppressPromptAt, settledStableIDs map[string]bool) []dialect.Finding {
	seen := map[string]bool{}
	structuredAtLocation := map[string]bool{}
	for _, finding := range in {
		if finding.Source != "review_prompt" && finding.Path != "" && finding.Line > 0 {
			structuredAtLocation[dialect.NormalizeBotName(finding.Bot)+"|"+finding.Path+"|"+strconv.Itoa(finding.Line)] = true
		}
	}
	out := []dialect.Finding{}
	for _, finding := range in {
		finding.Body = strings.TrimSpace(finding.Body)
		finding.Title = strings.TrimSpace(finding.Title)
		labels := dialect.ReviewLabelsFor(finding.Bot, finding.Body)
		if labels.Category != "" {
			finding.Category = labels.Category
		}
		if labels.Severity != "" {
			finding.Severity = labels.Severity
		}
		if labels.Scale != "" {
			finding.Scale = labels.Scale
		}
		if labels.Effort != "" {
			finding.Effort = labels.Effort
		}
		if !dialect.IsActionableFinding(finding) {
			continue
		}
		if finding.Source == "review_prompt" {
			key := dialect.NormalizeBotName(finding.Bot) + "|" + finding.Path + "|" + strconv.Itoa(finding.Line)
			if structuredAtLocation[key] || suppressPromptAt[key] {
				continue
			}
		}
		key := dialect.NormalizeBotName(finding.Bot) + "|" + finding.Path + "|" + strconv.Itoa(finding.Line) + "|" + finding.Title + "|" + finding.Body + "|" + finding.ThreadID
		// A bot-stable identity marker (Bugbot's BUG_ID) supersedes the
		// location/title key: the same bug re-reported in a new thread after a
		// push must collapse to one finding.
		if co, ok := dialect.CoReviewerByName(finding.Bot); ok && co.FindingDedupeKey != nil {
			if stable, ok := co.FindingDedupeKey(finding.Body); ok {
				if settledStableIDs[dialect.NormalizeBotName(finding.Bot)+"|"+stable] {
					continue // settled in some thread — every sibling is settled
				}
				key = dialect.NormalizeBotName(finding.Bot) + "|" + stable
			}
		}
		sum := sha256.Sum256([]byte(key))
		finding.ID = hex.EncodeToString(sum[:])
		if seen[finding.ID] {
			continue
		}
		seen[finding.ID] = true
		out = append(out, finding)
	}
	return out
}

// skipNoticeSource marks the synthetic finding Feedback builds from a "Review
// skipped" notice. It is the only finding with no thread to resolve and no way
// to be addressed except by changing the PR itself, so it is the one finding
// the pre-enqueue fix-first gate exempts — identified by this source rather than by its
// own display text, which names whichever bot posted it.
const skipNoticeSource = "review_skipped"

// excludeSkipNotice drops the synthetic skip finding from a blocking set.
func excludeSkipNotice(findings []dialect.Finding) []dialect.Finding {
	out := findings[:0:0]
	for _, f := range findings {
		if f.Source == skipNoticeSource {
			continue
		}
		out = append(out, f)
	}
	return out
}

// skipPredatesHead reports whether a skip notice was posted before the current
// head existed. A notice naming no commit cannot be bound by SHA, so it falls
// back to the same head-commit cutoff every other issue comment uses — without
// it a stale skip keeps surfacing as a blocking, thread-less finding and the
// loop exits 10 forever without ever waiting for a review.
func skipPredatesHead(comment ghapi.IssueComment, headCutoff func() time.Time) bool {
	if dialect.ReviewSkippedHeadSHA(comment.Body) != "" {
		return false // names its commit: SHA binding already decided this
	}
	cutoff := headCutoff()
	if cutoff.IsZero() {
		return false
	}
	// CodeRabbit produces some skip notices by EDITING its existing top-summary
	// comment, whose CreatedAt long predates the current head while UpdatedAt is
	// current. Judging by creation time discarded exactly those, so the finding
	// vanished while the engine still (correctly) read the primary as
	// unavailable. Use the later of the two, matching BotEvent.ObservedTime.
	at := comment.CreatedAt
	if comment.UpdatedAt.After(at) {
		at = comment.UpdatedAt
	}
	return at.Before(cutoff)
}

// skipAppliesToHead reports whether a "Review skipped" notice concerns the
// head under review. The refusal is per-head and narrowing the PR is the fix,
// so a skip of an EARLIER head must not keep surfacing: Loop treats findings as
// blocking before it enqueues, which would stop the replacement head from ever
// being submitted — the repair would permanently disable the reviewer.
func skipAppliesToHead(body, head string) bool {
	skipped := dialect.ReviewSkippedHeadSHA(body)
	if skipped == "" || head == "" {
		return true // names no commit: read conservatively as current
	}
	return dialect.SHAPrefixMatch(skipped, head)
}

// coReviewerStatuses summarizes each enabled co-reviewer's observed state for
// the current head: head review evidence, check-run state, and Macroscope's
// approvability verdict. Informational only — nothing here gates convergence.
func coReviewerStatuses(cfg Config, obs engine.Observation, since time.Time) map[string]CoReviewerStatus {
	if len(cfg.CoBots) == 0 {
		return nil
	}
	out := map[string]CoReviewerStatus{}
	for _, cb := range cfg.CoBots {
		key := dialect.NormalizeBotName(cb.Login)
		status := CoReviewerStatus{Reviewed: engine.CoReviewedHead(obs, cb.Login)}
		inProgress, clean, done, unable, failed := false, false, false, false, false
		for _, c := range obs.Checks {
			if dialect.NormalizeBotName(c.Bot) != key {
				continue
			}
			switch c.Verdict {
			case dialect.CheckInProgress:
				inProgress = true
			case dialect.CheckDoneClean:
				clean = true
			case dialect.CheckDone:
				done = true
			case dialect.CheckUnable:
				unable = true
			case dialect.CheckFailed:
				failed = true
			}
		}
		switch {
		case inProgress:
			status.CheckState = "in_progress"
		case clean:
			status.CheckState = "clean"
		case done:
			status.CheckState = "issues"
		// A bot that could not review at all (Macroscope's billing-issue skip) or
		// whose run crashed used to report "unknown", which reads as "nothing has
		// happened yet" — the operator waits for a review that is never coming
		// instead of fixing the workspace.
		case unable:
			status.CheckState = "unable"
		case failed:
			status.CheckState = "failed"
		default:
			status.CheckState = "unknown"
		}
		var latest time.Time
		for _, ev := range obs.Events {
			if ev.Kind != dialect.EvCoVerdict || ev.Approved == nil {
				continue
			}
			// Verdicts are documented as current-head state. Between a push and
			// the bot's new verdict the newest comment on the PR describes the
			// PREVIOUS head, so anything older than this round is not reported.
			if !since.IsZero() && ev.ObservedTime().Before(since) {
				continue
			}
			if dialect.NormalizeBotName(firstNonEmpty(ev.For, ev.Bot)) != key {
				continue
			}
			if at := ev.ObservedTime(); latest.IsZero() || at.After(latest) {
				latest = at
				if *ev.Approved {
					status.Verdict = "approved"
				} else {
					status.Verdict = "needs_human_review"
				}
			}
		}
		out[key] = status
	}
	return out
}

func isCurrentCodexThumbsUp(reaction ghapi.Reaction, since time.Time) bool {
	if !dialect.IsCodexBot(reaction.User.Login) || reaction.Content != "+1" {
		return false
	}
	return reaction.CreatedAt.IsZero() || notBefore(reaction.CreatedAt, since)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func (r FeedbackReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
