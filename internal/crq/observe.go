package crq

import (
	"context"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// observation bundles the pure engine.Observation the decision functions
// consume with the raw GitHub payloads Feedback's findings extraction needs, so
// a single fetch serves both the daemon (Pump: DecideFire/Progress) and the
// loop (Feedback: engine.Completion + finding parsing) without a second path.
type observation struct {
	eng      engine.Observation
	pull     ghapi.Pull
	reviews  []ghapi.Review
	comments []ghapi.IssueComment
}

// observe is the single place that asks GitHub "what happened on this PR" and
// reduces it to an engine.Observation (plus the raw reviews/comments Feedback
// parses into findings). The daemon's Pump builds it for the slot round
// (Progress) and the next-eligible round (DecideFire); Feedback builds it for
// the current round — so the "is head reviewed?" duplication of v2 collapses to
// one implementation.
//
// round anchors the round-relative facts: reactions target its fired command,
// the adoption cutoff is its LastAttemptAt, and reactions/thumbs-up are fetched
// only for a round that has fired. posted restores the chronological position
// of crq-authored trigger comments that Tidy has removed from GitHub.
func (s *Service) observe(ctx context.Context, cfg Config, repo string, pr int, round *Round, posted []engine.CommandComment, now time.Time) (observation, error) {
	pull, err := s.gh.GetPull(ctx, repo, pr)
	if err != nil {
		return observation{}, err
	}
	o := observation{pull: pull}
	o.eng.Open = pull.State == "open" && !pull.Merged
	if o.eng.Open && len(pull.Head.SHA) >= 9 {
		o.eng.Head = pull.Head.SHA[:9]
	}
	// The head's commit time bounds evidence that names no commit of its own
	// (a SHA-less "Review skipped"), so such a notice cannot suppress a later
	// head forever. Best-effort: an unreadable commit leaves it zero and the
	// engine falls back to accepting the notice, the conservative reading.
	if o.eng.Open && pull.Head.SHA != "" {
		if c, cerr := s.gh.GetCommit(ctx, repo, pull.Head.SHA); cerr == nil {
			o.eng.HeadAt = c.Committer.Date.UTC()
		}
	}

	// Reviews and issue comments are fetched even for a closed PR: the daemon's
	// Progress/DecideFire abandon it regardless, but Feedback still surfaces its
	// findings, and the extra two reads on a to-be-dropped round are negligible.
	reviews, err := s.gh.ListReviews(ctx, repo, pr)
	if err != nil {
		return observation{}, err
	}
	o.reviews = reviews
	for _, review := range reviews {
		// CodeRabbit submits empty-bodied COMMENTED review objects as carriers
		// for its inline-comment batches, minutes before the real review (the
		// one with an "Actionable comments posted" body) lands. A shell is not
		// review evidence: counting one converged a round with zero findings at
		// 17:26 while the real review was still posting until 17:32. Scope the
		// filter to the configured reviewer — only CodeRabbit posts these
		// carriers, and dropping another bot's empty review could discard real
		// evidence a Codex-gated round waits on.
		if cfg.isConfiguredBot(review.User.Login) && strings.TrimSpace(review.Body) == "" && strings.EqualFold(review.State, "COMMENTED") {
			continue
		}
		o.eng.Reviews = append(o.eng.Reviews, engine.ReviewSeen{
			Bot:         review.User.Login,
			ReviewID:    review.ID,
			Commit:      dialect.ShortOID(review.CommitID),
			SubmittedAt: review.SubmittedAt,
		})
	}

	comments, err := s.gh.ListIssueComments(ctx, repo, pr)
	if err != nil {
		return observation{}, err
	}
	o.comments = comments
	classifier := dialect.Classifier{
		CodeRabbit:    s.cr,
		Bot:           cfg.Bot,
		ReviewCommand: cfg.ReviewCommand,
		Primary:       cfg.classifierPrimary(),
		CoReviewers:   cfg.classifierCoReviewers(),
	}
	presentComments := make(map[int64]bool, len(comments))
	for _, c := range comments {
		presentComments[c.ID] = true
		o.eng.Events = append(o.eng.Events, classifier.Classify(c.User.Login, c.Body, c.ID, c.CreatedAt, c.UpdatedAt))
	}
	// Tidy removes spent trigger comments, but command/reply pairing still needs
	// their place in the chronological FIFO. PostedCommands is the persisted
	// proof of the comments crq wrote, so restore only commands no longer on the
	// PR; a live comment remains classified from its actual body above.
	for _, cmd := range posted {
		if presentComments[cmd.ID] {
			continue
		}
		ev := dialect.BotEvent{
			Kind:      dialect.EvCoCommand,
			CommentID: cmd.ID,
			CreatedAt: cmd.CreatedAt,
			For:       dialect.NormalizeBotName(cmd.Bot),
		}
		if dialect.NormalizeBotName(cmd.Bot) == dialect.NormalizeBotName(cfg.Bot) {
			ev.Kind = dialect.EvCommand
			ev.For = ""
		}
		o.eng.Events = append(o.eng.Events, ev)
	}

	// Check runs are the only place a silent-clean Bugbot round exists, and they
	// also suppress selfheal triggers and inform pre-fire decisions — so they are
	// fetched for any open PR with an enabled check-bearing co-reviewer, NOT
	// gated on FiredAt. An unfetchable check degrades to the bounded co-review
	// wait rather than failing the observation; the log line is the operator's
	// signal that a required check-bearing bot may time out spuriously.
	checksUnknown := false
	if o.eng.Open && pull.Head.SHA != "" && cfg.coChecksRelevant() {
		runs, cerr := s.gh.ListCheckRuns(ctx, repo, pull.Head.SHA)
		if cerr != nil {
			// Record the uncertainty rather than letting "no checks returned"
			// masquerade as "the bot has not started": that read posts a trigger
			// over a run already in flight.
			checksUnknown = true
			if s.log != nil {
				s.log.Printf("warning: check runs unavailable for %s#%d@%s: %v (co-reviewer triggers suppressed this pass)", repo, pr, dialect.ShortOID(pull.Head.SHA), cerr)
			}
		} else {
			for _, run := range runs {
				login, verdict := dialect.ClassifyCheckRun(run.App.Slug, run.Name, run.Output.Title, run.Output.Summary, run.Status, run.Conclusion)
				if verdict == dialect.CheckUnrelated || !cfg.coBotEnabled(login) {
					continue
				}
				o.eng.Checks = append(o.eng.Checks, engine.CheckSeen{Bot: login, Name: run.Name, Verdict: verdict, CompletedAt: run.CompletedAt})
			}
		}
	}

	// Reactions and Codex thumbs-up only matter for a round that has fired.
	if round != nil && round.FiredAt != nil {
		cutoff := round.FiredAt.UTC()
		// A completed round remains as the reviewed-head marker after Tidy
		// deletes its trigger, and an adopted command can be removed by its
		// author too. Reactions on a missing issue comment return 404, and no
		// reaction can appear after the comment has gone, so skip that read.
		if round.CommandID != 0 && presentComments[round.CommandID] {
			reactions, err := s.gh.ListCommentReactions(ctx, repo, round.CommandID)
			if err != nil {
				return observation{}, err
			}
			for _, reaction := range reactions {
				if cfg.isConfiguredBot(reaction.User.Login) {
					o.eng.Reacted = true
				}
				if isCurrentCodexThumbsUp(reaction, cutoff) {
					o.eng.CodexThumbsUp = true
				}
			}
		}
		if !o.eng.CodexThumbsUp && cfg.codexRelevant(o.eng) {
			reactions, err := s.gh.ListIssueReactions(ctx, repo, pr)
			if err != nil {
				return observation{}, err
			}
			for _, reaction := range reactions {
				if isCurrentCodexThumbsUp(reaction, cutoff) {
					o.eng.CodexThumbsUp = true
					break
				}
			}
		}
	}

	// Co-reviewer activity is derived from the same snapshot: whether each bot
	// reviews the PR unprompted (drives the fire decision) and whether it
	// participates in the current round (drives the dynamic completion gate).
	if len(cfg.CoBots) > 0 {
		o.eng.Co = map[string]engine.CoSeen{}
		for _, cb := range cfg.CoBots {
			seen := engine.CoSeen{AutoActive: engine.CoAutoActive(o.eng, cb.Login)}
			if co, ok := dialect.CoReviewerByName(cb.Name); ok && co.AppSlug != "" {
				seen.ChecksUnknown = checksUnknown
			}
			if round != nil {
				seen.ActiveThisRound = engine.CoActiveThisRound(*round, o.eng, cb.Login)
			}
			o.eng.Co[dialect.NormalizeBotName(cb.Login)] = seen
		}
	}

	// Adoptable commands are only consulted for a fire-eligible round.
	if round != nil && round.FireEligible(now) {
		cr, co, err := s.reviewCommands(ctx, cfg, repo, pr, o.eng, adoptCutoff(*round), pull, comments, reviews)
		if err != nil {
			return observation{}, err
		}
		o.eng.Commands = cr
		for key, cmds := range co {
			if entry, ok := o.eng.Co[key]; ok {
				entry.Commands = cmds
				o.eng.Co[key] = entry
			}
		}
	}
	return o, nil
}

// classifierPrimary returns registry wording hooks when the configured primary
// is itself a known reviewer. CodeRabbit has no registry entry and continues
// through the dedicated CodeRabbit classifier.
func (c Config) classifierPrimary() *dialect.CoReviewer {
	primary, ok := dialect.CoReviewerByName(c.Bot)
	if !ok {
		return nil
	}
	primary.Command = c.ReviewCommand
	return &primary
}

// classifierCoReviewers resolves the enabled registry entries with their
// config-resolved trigger commands.
func (c Config) classifierCoReviewers() []dialect.CoReviewer {
	out := make([]dialect.CoReviewer, 0, len(c.CoBots))
	for _, cb := range c.CoBots {
		co, ok := dialect.CoReviewerByName(cb.Name)
		if !ok {
			continue
		}
		co.Command = cb.Command
		out = append(out, co)
	}
	return out
}

// coChecksRelevant reports whether any enabled co-reviewer owns check runs,
// so the extra REST fetch (ETag'd — repeat polls are 304s) is only spent when
// a bot's evidence can live there.
func (c Config) coChecksRelevant() bool {
	if primary := c.classifierPrimary(); primary != nil && primary.AppSlug != "" {
		return true
	}
	for _, cb := range c.CoBots {
		if co, ok := dialect.CoReviewerByName(cb.Name); ok && co.AppSlug != "" {
			return true
		}
	}
	return false
}

// coBotEnabled reports whether login is one of the enabled co-reviewers.
func (c Config) coBotEnabled(login string) bool {
	if primary := c.classifierPrimary(); primary != nil && primary.Is(login) {
		return true
	}
	for _, cb := range c.CoBots {
		if dialect.NormalizeBotName(cb.Login) == dialect.NormalizeBotName(login) {
			return true
		}
	}
	return false
}

// adoptCutoff is the earliest command timestamp a round may adopt: the most
// recent failed/abandoned attempt, so a stale command from before a requeue is
// never adopted.
func adoptCutoff(r Round) time.Time {
	if r.LastAttemptAt != nil {
		return r.LastAttemptAt.UTC()
	}
	return time.Time{}
}

// codexRelevant reports whether Codex participates in this round, so the extra
// issue-reactions fetch for a Codex thumbs-up is only spent when it can matter.
func (c Config) codexRelevant(obs engine.Observation) bool {
	if dialect.HasCodexBot(c.RequiredBots) {
		return true
	}
	for _, review := range obs.Reviews {
		if dialect.IsCodexBot(review.Bot) {
			return true
		}
	}
	for _, ev := range obs.Events {
		if ev.Kind == dialect.EvCoClean || dialect.IsCodexBot(ev.Bot) {
			return true
		}
	}
	return false
}

// reviewCommands ports v2's existingReviewCommand and extends it to the
// co-reviewers. It returns the newest CodeRabbit command safe to adopt as an
// already-posted fire (cr) and each co-reviewer's live trigger commands
// present for the head, keyed by normalized login. All share ONE cutoff
// computation (LastAttemptAt floor, head-commit date, force-push) so a stale
// command from a previous head is excluded everywhere, and the head-guard/
// cutoff lookups are skipped entirely when no command is on the PR.
func (s *Service) reviewCommands(ctx context.Context, cfg Config, repo string, pr int, obs engine.Observation, notBeforeCutoff time.Time, pull ghapi.Pull, comments []ghapi.IssueComment, reviews []ghapi.Review) (cr []engine.CommandSeen, co map[string][]engine.CommandSeen, err error) {
	command := strings.TrimSpace(cfg.ReviewCommand)
	hasCR := command != "" && hasCommentBody(comments, command)
	coBodies := cfg.coCommandBodies()
	present := map[string][]string{}
	for key, bodies := range coBodies {
		for _, body := range bodies {
			if hasCommentBody(comments, body) {
				present[key] = bodies
				break
			}
		}
	}
	if !hasCR && len(present) == 0 {
		return nil, nil, nil
	}
	cutoff := notBeforeCutoff
	if pull.Head.SHA != "" {
		if dialect.ShortOID(pull.Head.SHA) != obs.Head {
			return nil, nil, nil
		}
		commit, gerr := s.gh.GetCommit(ctx, repo, pull.Head.SHA)
		if gerr != nil {
			if _, ok := ghapi.ThrottleWait(gerr); ok {
				return nil, nil, gerr
			}
			// No head-commit cutoff available (unreadable/404 head): skip adoption
			// rather than wedge the queue — the worst case is posting a command that
			// already exists, the pre-adoption behavior.
			return nil, nil, nil
		}
		if commit.Committer.Date.After(cutoff) {
			cutoff = commit.Committer.Date
		}
	}
	// A force-push can point the PR at a commit object whose committer date
	// predates commands made for an earlier head, so any command older than the
	// last force-push belongs to a previous head and must not be adopted. Without
	// this guard an old-head command could be adopted after a force-push, marking
	// an unreviewed head fired — so if the lookup is unavailable, skip adoption
	// this pass. The worst case then is posting a command that already exists, the
	// documented-safe pre-adoption fallback.
	fp, err := s.headForcePushCutoff(ctx, repo, pr)
	if err != nil {
		return nil, nil, nil
	}
	if fp.After(cutoff) {
		cutoff = fp
	}
	if hasCR {
		cr = s.adoptableCR(cfg, obs, cutoff, command, comments, reviews)
	}
	for key, bodies := range present {
		if cmds := adoptableCo(obs, key, cutoff, bodies, comments, reviews); len(cmds) > 0 {
			if co == nil {
				co = map[string][]engine.CommandSeen{}
			}
			co[key] = cmds
		}
	}
	return cr, co, nil
}

// coCommandBodies maps each triggerable co-reviewer (normalized login) to the
// comment bodies that count as its trigger: the config-resolved command plus
// the registry's alternate spellings (`bugbot run` / `cursor review`).
func (c Config) coCommandBodies() map[string][]string {
	out := map[string][]string{}
	for _, cb := range c.CoBots {
		var bodies []string
		add := func(body string) {
			body = strings.TrimSpace(body)
			if body == "" {
				return
			}
			for _, have := range bodies {
				if have == body {
					return
				}
			}
			bodies = append(bodies, body)
		}
		add(cb.Command)
		if co, ok := dialect.CoReviewerByName(cb.Name); ok {
			for _, alias := range co.TriggerAliases {
				add(alias)
			}
		}
		if len(bodies) > 0 {
			out[dialect.NormalizeBotName(cb.Login)] = bodies
		}
	}
	return out
}

// adoptableCo returns the newest trigger command comment present for the head
// that the co-reviewer has NOT already answered — with a review, or (Bugbot,
// Macroscope) a completed check run. A command answered by later evidence
// belongs to a finished round for an earlier head: on a regular push whose
// commit date predates that old command, the command survives the cutoff yet
// is already consumed, so treating it as live makes DecideCoPost see
// commandPresent=true and suppress the trigger the new head still needs.
// Mirrors adoptableCR's review-answered guard.
func adoptableCo(obs engine.Observation, loginKey string, cutoff time.Time, bodies []string, comments []ghapi.IssueComment, reviews []ghapi.Review) []engine.CommandSeen {
	var best []engine.CommandSeen
	var bestAt time.Time
	for _, body := range bodies {
		if got := newestCommandSince(body, cutoff, comments); len(got) > 0 {
			at := got[0].CreatedAt
			if at.IsZero() {
				at = got[0].UpdatedAt
			}
			if best == nil || at.After(bestAt) {
				best, bestAt = got, at
			}
		}
	}
	if len(best) == 0 {
		return nil
	}
	for _, review := range reviews {
		if dialect.NormalizeBotName(review.User.Login) == loginKey && !review.SubmittedAt.Before(bestAt) {
			return nil
		}
	}
	for _, check := range obs.Checks {
		if dialect.NormalizeBotName(check.Bot) == loginKey &&
			(check.Verdict == dialect.CheckDone || check.Verdict == dialect.CheckDoneClean) &&
			!check.CompletedAt.Before(bestAt) {
			return nil
		}
	}
	return best
}

// adoptableCR returns the newest CodeRabbit command comment safe to adopt as an
// already-posted fire, or none. A command the bot already answered with a review
// or a completion reply belongs to a finished round for an earlier head and is
// never adopted (adopting it would mark the new head fired without reviewing it).
func (s *Service) adoptableCR(cfg Config, obs engine.Observation, cutoff time.Time, command string, comments []ghapi.IssueComment, reviews []ghapi.Review) []engine.CommandSeen {
	best := newestCommandSince(command, cutoff, comments)
	if len(best) == 0 {
		return nil
	}
	bestAt := best[0].CreatedAt
	if bestAt.IsZero() {
		bestAt = best[0].UpdatedAt
	}
	for _, review := range reviews {
		if cfg.isConfiguredBot(review.User.Login) && !review.SubmittedAt.Before(bestAt) {
			return nil
		}
	}
	if engine.CommandHasCompletionReply(obs, cfg.policy(), best[0].ID) {
		return nil
	}
	return best
}

// newestCommandSince returns the newest comment whose trimmed body is command
// and which is not older than cutoff, as a single-element CommandSeen slice
// (empty when none).
func newestCommandSince(command string, cutoff time.Time, comments []ghapi.IssueComment) []engine.CommandSeen {
	var best ghapi.IssueComment
	var bestAt time.Time
	ok := false
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) != command {
			continue
		}
		when := comment.CreatedAt
		if when.IsZero() {
			when = comment.UpdatedAt
		}
		if !cutoff.IsZero() && when.Before(cutoff) {
			continue
		}
		if !ok || when.After(bestAt) {
			best = comment
			bestAt = when
			ok = true
		}
	}
	if !ok {
		return nil
	}
	return []engine.CommandSeen{{ID: best.ID, CreatedAt: best.CreatedAt, UpdatedAt: best.UpdatedAt}}
}

// hasCommentBody reports whether any comment's trimmed body equals body.
func hasCommentBody(comments []ghapi.IssueComment, body string) bool {
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) == body {
			return true
		}
	}
	return false
}

type headForcePush struct {
	at   time.Time
	head string
}

// latestHeadForcePush returns when the PR head was last force-pushed and the
// commit that event installed. A zero value means the PR has no such event.
func (s *Service) latestHeadForcePush(ctx context.Context, repo string, pr int) (headForcePush, error) {
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return headForcePush{}, nil
	}
	var result struct {
		Repository struct {
			PullRequest struct {
				TimelineItems struct {
					Nodes []struct {
						CreatedAt   time.Time `json:"createdAt"`
						AfterCommit struct {
							OID string `json:"oid"`
						} `json:"afterCommit"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	query := `query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$number) {
      timelineItems(itemTypes: HEAD_REF_FORCE_PUSHED_EVENT, last: 1) {
        nodes { ... on HeadRefForcePushedEvent { createdAt afterCommit { oid } } }
      }
    }
  }
}`
	if err := s.gh.GraphQL(ctx, query, map[string]any{"owner": owner, "name": name, "number": pr}, &result); err != nil {
		return headForcePush{}, err
	}
	nodes := result.Repository.PullRequest.TimelineItems.Nodes
	if len(nodes) == 0 {
		return headForcePush{}, nil
	}
	latest := nodes[len(nodes)-1]
	return headForcePush{at: latest.CreatedAt.UTC(), head: dialect.ShortOID(latest.AfterCommit.OID)}, nil
}

// headForcePushCutoff returns when the PR head was last force-pushed (zero when
// never), or an error when the lookup could not run. The caller must not adopt a
// command without this guard: a lookup error means the force-push protection is
// unavailable, so adoption is skipped rather than done blind.
func (s *Service) headForcePushCutoff(ctx context.Context, repo string, pr int) (time.Time, error) {
	fp, err := s.latestHeadForcePush(ctx, repo, pr)
	return fp.at, err
}
