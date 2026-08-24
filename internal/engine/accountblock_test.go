package engine

import (
	"testing"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
	"github.com/kristofferR/codereview-queue/internal/state"
)

// The account allowance is not a property of a round. Progress only derived the
// window for a round that still existed and fired before the notice, and neither
// survives a fix session's push: the head moves, the round is superseded, and
// the rate-limit reply is archived unread. crq then believed the account was
// free and posted the command again minutes after being told to wait.
func TestObservedAccountBlockSurvivesASupersededRound(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	window := now.Add(38 * time.Minute)

	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now.Add(-4 * time.Minute), Window: &window,
	}}}

	// No round is involved at all — the one that asked has been superseded away.
	blk := ObservedAccountBlock(obs, p, state.AccountQuota{}, now)
	if blk == nil {
		t.Fatal("a rate-limit notice must count even with no round to attach it to")
	}
	if !blk.Until.Equal(window) {
		t.Errorf("until = %s, want the window the bot stated (%s)", blk.Until, window)
	}
}

// CodeRabbit rewrites its notice in place as the window counts down. Treating
// each edit as new evidence renews the block forever from a message crq has
// already accounted for — and the standing window would never be allowed to end.
func TestObservedAccountBlockIgnoresAnEditedNotice(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	recorded := now.Add(-30 * time.Minute)
	blocked := now.Add(30 * time.Minute)

	q := state.AccountQuota{BlockedUntil: &blocked, RLCommentID: 900, RLCommentUpdated: &recorded}
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now, // edited just now
	}}}

	if blk := ObservedAccountBlock(obs, p, q, now); blk != nil {
		t.Errorf("an edit of the recorded notice created a new block until %s", blk.Until)
	}

	// A DIFFERENT notice is new evidence and does block.
	obs.Events[0].CommentID = 901
	if blk := ObservedAccountBlock(obs, p, q, now); blk == nil {
		t.Error("a new rate-limit notice must block the account")
	}
}

func TestObservedAccountBlockAcceptsAReusedCommentAfterTheOldBlock(t *testing.T) {
	now := time.Date(2026, 7, 27, 2, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	recorded := now.Add(-40 * time.Minute)
	expired := now.Add(-10 * time.Minute)
	nextWindow := now.Add(45 * time.Minute)
	q := state.AccountQuota{
		BlockedUntil: &expired, RLCommentID: 900, RLCommentUpdated: &recorded,
	}
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now, Window: &nextWindow,
	}}}

	blk := ObservedAccountBlock(obs, p, q, now)
	if blk == nil {
		t.Fatal("a later fire's update of the reused comment was treated as permanently spent")
	}
	if !blk.Until.Equal(nextWindow) {
		t.Errorf("until = %s, want the reused comment's new window %s", blk.Until, nextWindow)
	}
}

// A notice is spent when crq has accounted for THAT NOTICE, which is not what a
// calibration timestamp says: Account.CheckedAt is advanced by a probe still
// awaiting its reply, and by one whose reply had no parseable reset in it.
// Neither is evidence the account is clear, and both used to discard a notice
// crq had never seen — after which the eligible round fires inside the block the
// bot had just reported.
func TestObservedAccountBlockOutlivesAnInconclusiveCalibration(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	checked := now.Add(-time.Minute) // the probe ran after the notice was posted

	// No window parsed out of it, so only the watermark decides.
	q := state.AccountQuota{CheckedAt: &checked}
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now.Add(-4 * time.Minute),
	}}}

	blk := ObservedAccountBlock(obs, p, q, now)
	if blk == nil {
		t.Fatal("a notice crq never accounted for was discarded; the round fires inside the block")
	}
	if !blk.Until.Equal(obs.Events[0].UpdatedAt.Add(p.rateLimitFallback())) {
		t.Errorf("until = %s, want the fallback window", blk.Until)
	}

	// Once accounted for, the same notice is spent — otherwise every observation
	// of it starts another fallback window and the block never ends.
	seen := blk.CommentUpdated
	q = state.AccountQuota{CheckedAt: &checked, RLCommentID: 900, RLCommentUpdated: &seen}
	if blk := ObservedAccountBlock(obs, p, q, now); blk != nil {
		t.Errorf("an accounted notice blocked again until %s", blk.Until)
	}
}

func TestObservedAccountBlockDoesNotRenewFromAlternatingHistoricalNotices(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 30, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	first := now.Add(-40 * time.Minute)
	second := now.Add(-30 * time.Minute)

	for _, notice := range []dialect.BotEvent{
		{Kind: dialect.EvRateLimited, Bot: p.Bot, CommentID: 900, UpdatedAt: first},
		{Kind: dialect.EvRateLimited, Bot: p.Bot, CommentID: 901, UpdatedAt: second},
	} {
		obs := Observation{Events: []dialect.BotEvent{notice}}
		if blk := ObservedAccountBlock(obs, p, state.AccountQuota{}, now); blk != nil {
			t.Errorf("historical notice %d renewed the account block until %s", notice.CommentID, blk.Until)
		}
	}
}

func TestObservedAccountBlockIgnoresAnExpiredExplicitWindow(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	expired := now.Add(-time.Minute)
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 900, UpdatedAt: now.Add(-time.Hour), Window: &expired,
	}}}

	// A successful calibration may clear the notice watermark. The explicit
	// expired window still proves this notice is spent; only an unparseable
	// notice needs a conservative fallback.
	if blk := ObservedAccountBlock(obs, p, state.AccountQuota{}, now); blk != nil {
		t.Errorf("expired explicit window was revived until %s", blk.Until)
	}
}

// Only the configured primary meters the account; a co-reviewer saying it is
// busy is not an account block.
func TestObservedAccountBlockIgnoresOtherBots(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: dialect.CodexBotLogin, CommentID: 7, UpdatedAt: now,
	}}}
	if blk := ObservedAccountBlock(obs, p, state.AccountQuota{}, now); blk != nil {
		t.Errorf("a co-reviewer's limit blocked the account until %s", blk.Until)
	}
}

// Every PR gets its own rate-limit notice, and each one is told when THAT pull
// request may go again — so a notice from another PR routinely names an earlier
// moment than the window already standing. Applying it moved the whole fleet's
// block backwards, and the next fire landed inside a window the bot had not
// lifted.
func TestObservedAccountBlockIsNeverShortenedByAnotherNotice(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 7, 0, 0, time.UTC)
	p := Policy{Bot: "coderabbitai[bot]", RateLimitFallback: 15 * time.Minute}
	standing := now.Add(50 * time.Minute)
	shorter := now.Add(9 * time.Minute)

	obs := Observation{Events: []dialect.BotEvent{{
		Kind: dialect.EvRateLimited, Bot: "coderabbitai[bot]",
		CommentID: 901, UpdatedAt: now.Add(-1 * time.Minute), Window: &shorter,
	}}}
	q := state.AccountQuota{
		BlockedUntil: &standing,
		RLCommentID:  900, // a DIFFERENT notice, from the PR that is blocked longest
	}

	blk := ObservedAccountBlock(obs, p, q, now)
	if blk == nil {
		t.Fatal("a distinct notice is still evidence and must be accounted for")
	}
	if !blk.Until.Equal(standing) {
		t.Errorf("until = %s, want the standing window kept (%s)", blk.Until, standing)
	}
	// Recorded as accounted for, or the same expired message reads as unseen
	// evidence on a later pass and becomes a fresh fallback block of its own.
	if blk.CommentID != 901 {
		t.Errorf("comment = %d, want the notice that was just read (901)", blk.CommentID)
	}
}
