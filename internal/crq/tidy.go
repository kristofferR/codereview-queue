package crq

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/engine"
	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// TidyResult reports what a tidy pass removed, or would have.
type TidyResult struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	// Deleted are the comment IDs removed; Kept explains, per remaining
	// candidate, why it stayed — so a pass that deletes nothing says why rather
	// than looking broken.
	Deleted []int64 `json:"deleted"`
	// Failed are the deletions GitHub refused. Without them an empty Deleted
	// reads as "nothing was spent" when it may mean "the token may not delete",
	// and the caller has no way to tell the two apart.
	Failed []TidyFailure `json:"failed,omitempty"`
	Kept   []string      `json:"kept,omitempty"`
	DryRun bool          `json:"dry_run,omitempty"`
}

// TidyFailure is one comment a pass decided to remove and could not.
type TidyFailure struct {
	ID    int64  `json:"id"`
	Error string `json:"error"`
}

// Tidy removes the review-trigger comments crq posted that nothing needs any
// more: the bot answered them, and the round that asked has progressed.
//
// A PR driven through a dozen rounds accumulates a dozen "@coderabbitai review"
// comments and a dozen acknowledgements, which buries the conversation a human
// came to read. This deletes crq's own half of that.
//
// It is deliberately narrow in three ways.
//
// Only comments CRQ POSTED. A human's "@coderabbitai review" is someone's
// decision to ask, and not crq's to erase; the candidate list is built from the
// comments each round recorded WRITING (Round.PostedCommands), not by matching
// text and not from the round's CommandID — a round records an adopted command
// there too, and adoption is exactly how a person's request gets into it.
//
// Only comments that STILL READ as a trigger. A recorded ID says crq wrote that
// comment, not that it is still the one-line command crq wrote: anyone with
// write access can edit it into an explanatory note, and deleting that destroys
// someone's words rather than a spent request.
//
// Only crq's, not the bot's. An auto-generated reply can be a rate-limit notice
// or a skipped-review notice, which crq classifies as evidence and surfaces as
// a finding — so deleting bot comments is how this feature would quietly destroy
// feedback nobody had read yet.
func (s *Service) Tidy(ctx context.Context, repo string, pr int, dryRun bool) (TidyResult, error) {
	repo = NormalizeRepo(repo)
	result := TidyResult{Repo: repo, PR: pr, Deleted: []int64{}, DryRun: dryRun}

	st, _, err := s.store.Load(ctx)
	if err != nil {
		return result, err
	}
	// Which comments count as a trigger depends on who reviews, so the whole
	// path takes a configuration value rather than reading the Service's. That
	// is what lets per-repo reviewers substitute one here later without
	// threading anything new through.
	cfg := s.cfgFor(st, repo)
	observedPosted := collectPosted(st, repo, pr)
	if len(observedPosted.commands) == 0 {
		result.Kept = append(result.Kept, "no round on this pr posted a trigger comment")
		return result, nil
	}

	obs, err := s.observe(ctx, cfg, repo, pr, nil, observedPosted.commands, s.clock())
	if err != nil {
		return result, err
	}
	if !obs.eng.Open {
		// A closed PR is nobody's reading material, and deleting from it spends
		// writes for no one's benefit.
		result.Kept = append(result.Kept, "pr is closed")
		return result, nil
	}

	// Re-read state AFTER the observation. What follows is destructive, and the
	// snapshot above is old enough for another fleet member to have started a
	// round that adopted one of these comments — which would make this delete the
	// command that round is now waiting on.
	st, _, err = s.store.Load(ctx)
	if err != nil {
		return result, err
	}
	cfg = s.cfgFor(st, repo)
	posted := collectPosted(st, repo, pr)

	// A deleted comment stays on its round for ever, so without this every later
	// pass would DELETE it again and read the 404 as a fresh removal.
	present := map[int64]ghapi.IssueComment{}
	for _, comment := range obs.comments {
		present[comment.ID] = comment
	}
	triggers := cfg.triggerBodies()
	observedIDs := make(map[int64]bool, len(observedPosted.commands))
	for _, cmd := range observedPosted.commands {
		observedIDs[cmd.ID] = true
	}
	var commands []engine.CommandComment
	var missing []PostedCommand
	edited := 0
	for _, cmd := range posted.commands {
		comment, onPR := present[cmd.ID]
		if !onPR {
			// The state reload may include a command posted after observation.
			// Its absence from this older snapshot says nothing; the next pass
			// can classify it from a fresh comment list.
			if observedIDs[cmd.ID] {
				missing = append(missing, PostedCommand{ID: cmd.ID, Bot: cmd.Bot, At: cmd.CreatedAt})
			}
			continue
		}
		if !isTriggerBody(triggers, cmd.Bot, comment.Body) {
			edited++
			continue
		}
		commands = append(commands, cmd)
	}
	if edited > 0 {
		result.Kept = append(result.Kept, "a trigger comment crq posted no longer reads as one; someone edited it")
	}
	if len(commands) == 0 {
		if !dryRun && !s.cfg.DryRun {
			if err := s.recordTidyState(ctx, repo, pr, 0, missing); err != nil {
				return result, err
			}
		}
		result.Kept = append(result.Kept, "every trigger comment crq posted is already gone")
		return result, nil
	}

	adoptableFrom, err := s.adoptableFrom(ctx, repo, pr, obs.eng.HeadAt)
	if err != nil {
		return result, err
	}
	in := engine.TidyInput{
		Commands:      commands,
		Live:          posted.live,
		Superseded:    posted.superseded,
		AdoptableFrom: adoptableFrom,
	}
	cursor := st.TidyReactionCursors[QueueKey(repo, pr)]
	in.AnsweredAt, in.ReactionTargets, cursor, err = s.answered(ctx, repo, pr, obs, commands, posted, cursor)
	if err != nil {
		return result, err
	}
	stale := engine.StaleCommands(in)
	if len(stale) == 0 {
		if !dryRun && !s.cfg.DryRun {
			if err := s.recordTidyState(ctx, repo, pr, cursor, missing); err != nil {
				return result, err
			}
		}
		result.Kept = append(result.Kept, "every trigger comment is still live, unanswered, or still adoptable")
		return result, nil
	}
	if dryRun || s.cfg.DryRun {
		result.DryRun = true
		result.Deleted = stale
		return result, nil
	}

	byID := make(map[int64]PostedCommand, len(commands))
	for _, command := range commands {
		byID[command.ID] = PostedCommand{ID: command.ID, Bot: command.Bot, At: command.CreatedAt}
	}
	tombstones := append(make([]PostedCommand, 0, len(missing)+len(stale)), missing...)
	for _, id := range stale {
		tombstones = append(tombstones, byID[id])
	}
	// Persist the deleted command's FIFO position before the destructive write.
	// If this process dies after the state write, the still-present GitHub
	// comment suppresses the synthetic event; if it dies after the delete, the
	// tombstone already exists.
	if err := s.recordTidyState(ctx, repo, pr, cursor, tombstones); err != nil {
		return result, err
	}

	var (
		forget      []int64
		throttleErr error
	)
deleteLoop:
	for i, id := range stale {
		snapshot := present[id]
		latest, err := s.gh.GetIssueComment(ctx, repo, id)
		switch {
		case errors.Is(err, ghapi.ErrNotFound):
			// It disappeared after the observation. Keep its tombstone: a
			// delayed reply still needs the command's FIFO position.
			result.Deleted = append(result.Deleted, id)
			continue
		case err != nil:
			forget = append(forget, id)
			if ghapi.IsThrottled(err) {
				throttleErr = err
				forget = append(forget, stale[i+1:]...)
				break deleteLoop
			}
			result.Failed = append(result.Failed, TidyFailure{ID: id, Error: err.Error()})
			continue
		case latest.Body != snapshot.Body || !latest.UpdatedAt.Equal(snapshot.UpdatedAt):
			// Revalidate immediately before DELETE. An operator may have turned
			// the one-line trigger into a note while the observation, force-push
			// lookup, and reaction reads were in flight.
			forget = append(forget, id)
			result.Kept = append(result.Kept, "a trigger comment changed while tidy was running; its edited words were preserved")
			continue
		}

		err = s.gh.DeleteIssueComment(ctx, repo, id)
		switch {
		case err == nil, errors.Is(err, ghapi.ErrNotFound):
			// Already gone is the outcome we wanted. A recorded command can
			// vanish without crq: the bot removes some of its own command
			// comments, and a person may tidy by hand.
			result.Deleted = append(result.Deleted, id)
		default:
			if ghapi.IsThrottled(err) {
				forget = append(forget, id)
				throttleErr = err
				forget = append(forget, stale[i+1:]...)
				break deleteLoop
			}
			// A transport failure may arrive after GitHub applied the DELETE.
			// Keep the prewritten tombstone unless an API response proves the
			// comment remained on GitHub.
			var apiErr *ghapi.APIError
			if errors.As(err, &apiErr) {
				forget = append(forget, id)
			}
			// One write failing must not abandon the rest — but it is reported,
			// not just logged: a caller reading the result is the only one who can
			// act on "the token cannot delete these".
			result.Failed = append(result.Failed, TidyFailure{ID: id, Error: err.Error()})
			if s.log != nil {
				s.log.Printf("tidy: %s#%d comment %d: %v", repo, pr, id, err)
			}
		}
	}
	if len(forget) > 0 {
		if _, err := s.store.Update(ctx, func(st *State) error {
			st.ForgetTidied(repo, pr, forget...)
			return nil
		}); err != nil {
			return result, err
		}
	}
	if throttleErr != nil {
		return result, throttleErr
	}
	if s.log != nil && len(result.Deleted) > 0 {
		s.log.Printf("tidy: %s#%d removed %d spent trigger comment(s)", repo, pr, len(result.Deleted))
	}
	return result, nil
}

// postedCommands is one PR's trigger comments as tidying sees them.
type postedCommands struct {
	// commands are the comments crq POSTED, each with the reviewer it was
	// addressed to. A round's CommandID is not in here unless crq wrote it: an
	// adopted command is someone else's comment, and deleting a person's
	// "@coderabbitai review" is not crq's to do.
	commands []engine.CommandComment
	// live are the commands a round that has NOT progressed still depends on —
	// posted or adopted, because either is what crq would read instead of
	// posting again.
	live map[int64]bool
	// superseded are commands their own round replaced with a newer one, so
	// they can never be adopted again whatever phase that round is in.
	superseded map[int64]bool
	// firedOn maps a posted trigger to the primary command its round fired on,
	// when that is another comment. observe() reads a Codex thumbs-up from there
	// too, so answered() retains it while its reaction is completion evidence.
	firedOn map[int64]int64
}

// collectPosted gathers the trigger comments crq posted for repo#pr, from the
// open round and from every archived round of the same PR.
func collectPosted(st State, repo string, pr int) postedCommands {
	out := postedCommands{live: map[int64]bool{}, superseded: map[int64]bool{}, firedOn: map[int64]int64{}}
	seen := map[int64]bool{}
	appendCommand := func(command engine.CommandComment) {
		if command.ID == 0 || seen[command.ID] {
			return
		}
		seen[command.ID] = true
		out.commands = append(out.commands, command)
	}
	collect := func(r Round, progressed bool) {
		current := map[int64]bool{}
		if r.CommandID != 0 {
			current[r.CommandID] = true
		}
		for _, co := range r.CoBots {
			if co.CommandID != 0 {
				current[co.CommandID] = true
			}
		}
		if !progressed {
			for id := range current {
				out.live[id] = true
			}
		}
		for _, p := range r.PostedCommands {
			if !current[p.ID] {
				out.superseded[p.ID] = true
			}
			if r.CommandID != 0 && r.CommandID != p.ID {
				out.firedOn[p.ID] = r.CommandID
			}
			appendCommand(engine.CommandComment{
				ID: p.ID, Bot: dialect.NormalizeBotName(p.Bot), CreatedAt: p.At.UTC(),
			})
		}
	}
	if round := st.Round(repo, pr); round != nil {
		collect(*round, !round.Active())
	}
	for _, archived := range st.Archive {
		if NormalizeRepo(archived.Repo) == repo && archived.PR == pr {
			collect(archived, true)
		}
	}
	for _, command := range st.TidiedCommands[QueueKey(repo, pr)] {
		appendCommand(engine.CommandComment{
			ID: command.ID, Bot: dialect.NormalizeBotName(command.Bot), CreatedAt: command.At.UTC(),
		})
	}
	sort.Slice(out.commands, func(i, j int) bool {
		if !out.commands[i].CreatedAt.Equal(out.commands[j].CreatedAt) {
			return out.commands[i].CreatedAt.Before(out.commands[j].CreatedAt)
		}
		return out.commands[i].ID < out.commands[j].ID
	})
	return out
}

// triggerBodies maps each reviewer (normalized login) to the comment bodies
// that count as its trigger: the configured review command for the primary, the
// command plus registry aliases for each enabled co-reviewer.
func (c Config) triggerBodies() map[string][]string {
	out := c.coCommandBodies()
	if command := strings.TrimSpace(c.ReviewCommand); command != "" {
		key := dialect.NormalizeBotName(c.Bot)
		out[key] = append(out[key], command)
	}
	return out
}

// isTriggerBody reports whether body is still one of bot's trigger commands.
//
// The author is no help here — crq posts as the operator's own account, so
// crq's comment and that person's are written by the same login. The body is
// the only thing that distinguishes a spent one-line command from a comment
// someone has since edited into something they meant to keep.
//
// Unrecognised keeps the comment, which is also what happens when the trigger
// was reconfigured or its co-reviewer disabled after the comment went out: an
// unevaluable guard is not permission to delete.
func isTriggerBody(triggers map[string][]string, bot, body string) bool {
	body = strings.TrimSpace(body)
	for _, want := range triggers[dialect.NormalizeBotName(bot)] {
		if body == want {
			return true
		}
	}
	return false
}

// adoptableFrom is the moment a command stops being adoptable, which is what
// tidying may delete below: the head commit date raised to the last force-push.
//
// Adoption uses the later of the two on purpose — a force-push can point the PR
// at a commit object whose date predates commands made for an earlier head — so
// a tidy pass reading the commit date alone keeps an already-answered trigger
// that no round can ever adopt again, for ever once the PR merges. A zero headAt
// is the unevaluable guard that keeps every comment, and needs no lookup; a
// failed lookup falls back to the commit date, which only ever keeps more.
func (s *Service) adoptableFrom(ctx context.Context, repo string, pr int, headAt time.Time) (time.Time, error) {
	if headAt.IsZero() {
		return headAt, nil
	}
	fp, err := s.headForcePushCutoff(ctx, repo, pr)
	if err != nil {
		if ghapi.IsThrottled(err) {
			return headAt, err
		}
		if s.log != nil {
			s.log.Printf("tidy: %s#%d force-push lookup: %v", repo, pr, err)
		}
		return headAt, nil
	}
	if fp.After(headAt) {
		return fp, nil
	}
	return headAt, nil
}

// answered is answeredAt plus the evidence only a reaction carries.
//
// Codex can answer a trigger with nothing but a thumbs-up, and that reaction
// alone satisfies its gate and completes the round — so a round that ended that
// way leaves no review, event or check for answeredAt to find, and its
// "@codex review" comment would be kept for ever. observe() fetches reactions
// only for a round that has fired, and tidying observes with no round at all, so
// they are read here.
//
// All of observe's sources count, because each completes the round: the trigger
// comment itself, read once per candidate nothing else has answered; the PR —
// Codex answers a command in the PR description by reacting there — read at most
// once per pass; and the primary command the candidate's own round fired on,
// which is where observe() looks first. Each is read only while a candidate is
// still unanswered, so the ordinary pass pays for none of them.
const maxTidyReactionCandidates = 4

func (s *Service) answered(ctx context.Context, repo string, pr int, obs observation, commands []engine.CommandComment, posted postedCommands, cursor int64) (map[string]time.Time, map[int64]bool, int64, error) {
	out := answeredAt(obs)
	reactionTargets := map[int64]bool{}
	var candidates []engine.CommandComment
	for _, cmd := range commands {
		if posted.live[cmd.ID] || !dialect.IsCodexBot(cmd.Bot) || answeredSince(out, cmd) {
			continue
		}
		candidates = append(candidates, cmd)
	}
	if len(candidates) == 0 {
		return out, reactionTargets, cursor, nil
	}
	// A candidate may have been triggered beside another command and Codex may
	// have approved by reacting to that primary command. Until this bounded scan
	// identifies where the approval lives, conservatively retain every possible
	// target; otherwise an unscanned candidate could lose its evidence.
	possibleTargets := map[int64]int{}
	for _, cmd := range candidates {
		if fired := posted.firedOn[cmd.ID]; fired != 0 {
			possibleTargets[fired]++
			reactionTargets[fired] = true
		}
	}
	clearPossibleTarget := func(cmd engine.CommandComment) {
		fired := posted.firedOn[cmd.ID]
		if fired == 0 {
			return
		}
		possibleTargets[fired]--
		if possibleTargets[fired] == 0 {
			delete(reactionTargets, fired)
		}
	}
	start := 0
	foundCursor := false
	for i, cmd := range candidates {
		if cmd.ID == cursor {
			start = (i + 1) % len(candidates)
			foundCursor = true
			break
		}
	}
	// A successful prior pass may already have deleted the cursor's comment.
	// GitHub comment IDs increase chronologically, so resume at the first later
	// candidate instead of falling back to the oldest and rescanning it.
	if cursor != 0 && !foundCursor {
		for i, cmd := range candidates {
			if cmd.ID > cursor {
				start = i
				break
			}
		}
	}

	var onPR []ghapi.Reaction
	readPR := false
	scanned := 0
	for offset := 0; offset < len(candidates) && scanned < maxTidyReactionCandidates; offset++ {
		cmd := candidates[(start+offset)%len(candidates)]
		if answeredSince(out, cmd) {
			continue
		}
		scanned++
		cursor = cmd.ID
		reactions, err := s.gh.ListCommentReactions(ctx, repo, cmd.ID)
		if err != nil {
			if ghapi.IsThrottled(err) {
				return out, reactionTargets, cursor, err
			}
			// Housekeeping: an unreadable reaction keeps the comment, and the
			// next pass tries again.
			if s.log != nil {
				s.log.Printf("tidy: %s reactions on comment %d: %v", repo, cmd.ID, err)
			}
			continue
		}
		if noteThumbsUp(out, cmd, reactions) {
			reactionTargets[cmd.ID] = true
		}
		if answeredSince(out, cmd) {
			clearPossibleTarget(cmd)
			continue
		}
		if !readPR {
			readPR = true
			if onPR, err = s.gh.ListIssueReactions(ctx, repo, pr); err != nil {
				if ghapi.IsThrottled(err) {
					return out, reactionTargets, cursor, err
				}
				if s.log != nil {
					s.log.Printf("tidy: %s#%d reactions: %v", repo, pr, err)
				}
				onPR = nil
			}
		}
		noteThumbsUp(out, cmd, onPR)
		if answeredSince(out, cmd) {
			clearPossibleTarget(cmd)
			continue
		}
		// Last, the command this trigger's own round fired on. A round gated on
		// Codex completes on a thumbs-up left there too, and a round that ended
		// that way leaves its trigger looking like nobody ever read it.
		if fired := posted.firedOn[cmd.ID]; fired != 0 {
			reactions, err := s.gh.ListCommentReactions(ctx, repo, fired)
			if err != nil {
				if ghapi.IsThrottled(err) {
					return out, reactionTargets, cursor, err
				}
				if s.log != nil {
					s.log.Printf("tidy: %s reactions on comment %d: %v", repo, fired, err)
				}
				continue
			}
			if !noteThumbsUp(out, cmd, reactions) {
				clearPossibleTarget(cmd)
			}
		}
	}
	return out, reactionTargets, cursor, nil
}

// recordTidyState advances the bounded reaction scan and writes command
// tombstones in one CAS update. It avoids a state commit when neither changed.
func (s *Service) recordTidyState(ctx context.Context, repo string, pr int, cursor int64, commands []PostedCommand) error {
	key := QueueKey(repo, pr)
	_, err := s.store.Update(ctx, func(st *State) error {
		changed := false
		if cursor != 0 && st.TidyReactionCursors[key] != cursor {
			if st.TidyReactionCursors == nil {
				st.TidyReactionCursors = map[string]int64{}
			}
			st.TidyReactionCursors[key] = cursor
			changed = true
		}
		before := len(st.TidiedCommands[key])
		st.RecordTidied(repo, pr, commands...)
		if len(st.TidiedCommands[key]) != before {
			changed = true
		}
		if !changed {
			return ErrNoChange
		}
		return nil
	})
	return err
}

// answeredSince reports whether cmd's bot has demonstrably acted since cmd was
// posted, which is the evidence tidying needs before removing it.
func answeredSince(answered map[string]time.Time, cmd engine.CommandComment) bool {
	at, ok := answered[cmd.Bot]
	return ok && !at.Before(cmd.CreatedAt)
}

// noteThumbsUp records a Codex +1 among reactions as cmd's bot having acted and
// reports whether the comment carrying those reactions must be retained.
func noteThumbsUp(answered map[string]time.Time, cmd engine.CommandComment, reactions []ghapi.Reaction) bool {
	found := false
	for _, reaction := range reactions {
		if !isCurrentCodexThumbsUp(reaction, cmd.CreatedAt) {
			continue
		}
		found = true
		at := reaction.CreatedAt
		if at.IsZero() {
			at = cmd.CreatedAt
		}
		if at.After(answered[cmd.Bot]) {
			answered[cmd.Bot] = at
		}
	}
	return found
}

// answeredAt is the newest moment each reviewer demonstrably acted on this PR.
// A command with nothing after it was never read, and only what has been read is
// removed.
func answeredAt(obs observation) map[string]time.Time {
	out := map[string]time.Time{}
	note := func(bot string, at time.Time) {
		key := dialect.NormalizeBotName(bot)
		if key == "" || at.IsZero() {
			return
		}
		if at.After(out[key]) {
			out[key] = at
		}
	}
	for _, review := range obs.reviews {
		note(review.User.Login, review.SubmittedAt)
	}
	for _, event := range obs.eng.Events {
		note(event.Bot, eventAt(event))
	}
	for _, check := range obs.eng.Checks {
		note(check.Bot, check.CompletedAt)
	}
	return out
}

// eventAt is when a classified bot event happened.
func eventAt(e dialect.BotEvent) time.Time {
	if !e.UpdatedAt.IsZero() {
		return e.UpdatedAt
	}
	return e.CreatedAt
}

// tidyAfterPump removes spent trigger comments for a PR whose round just
// progressed, which is what makes tidying happen regularly without a sweep of
// its own: the daemon is already looking at this PR.
//
// It is best-effort by design. Deleting a comment is housekeeping, and a
// housekeeping failure must never break the pass that did the real work.
func (s *Service) tidyAfterPump(ctx context.Context, st State, res PumpResult) error {
	if res.Repo == "" || res.PR == 0 {
		return nil
	}
	switch res.Action {
	case "cleared", "deduped", "requeued", "skipped":
		// The round reached a state where its earlier commands are answered and
		// spent. "cleared" is the ordinary one — the round holding the slot
		// completed or was acknowledged — and leaving it out meant the common
		// successful round was never tidied at all. "fired" is not here: that
		// round is live and owns its command.
		//
		// Neither is "waiting", which is what a pump that changed NOTHING reports:
		// every poll of a long-running review would then re-observe the PR the
		// pump just observed — pull, commit, paginated reviews/comments, check
		// runs — so housekeeping would spend REST quota the queue needs, over and
		// over, for a PR whose comments are exactly as spent as they were last
		// poll. The one "waiting" that does move a round (a co-review wait) parks
		// it with its commands still live, so it has nothing of its own to remove;
		// anything older waits for the pass that clears it.
	default:
		return nil
	}
	return s.tidyProgressed(ctx, st, res.Repo, res.PR)
}

// tidyProgressed runs a tidy pass for one PR whose round just moved, and
// swallows ordinary failures: deleting a comment is housekeeping, and a
// housekeeping failure must never break the pass that did the real work.
// GitHub throttles are returned so autoreview can sleep through the reset
// window instead of continuing to spend requests that are expected to fail.
//
// The switch is resolved from the snapshot the pump decided on, not from the env
// this process started with: CRQ_TIDY is offered as a fleet setting, and reading
// the startup value made saving it in the dashboard change nothing on any daemon
// while the page reported fleet-wide tidying as on.
func (s *Service) tidyProgressed(ctx context.Context, st State, repo string, pr int) error {
	if !s.cfgFor(st, repo).Tidy {
		return nil
	}
	if _, err := s.Tidy(ctx, repo, pr, false); err != nil {
		if s.log != nil {
			s.log.Printf("tidy: %s#%d: %v", repo, pr, err)
		}
		if ghapi.IsThrottled(err) {
			return err
		}
	}
	return nil
}
