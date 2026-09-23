package engine

import "time"

// CommandComment is one review-trigger comment crq posted.
type CommandComment struct {
	ID        int64
	Bot       string
	CreatedAt time.Time
}

// TidyInput is everything the decision needs about one PR's trigger comments.
type TidyInput struct {
	// Commands are the trigger comments crq itself posted, oldest first.
	Commands []CommandComment
	// AnsweredAt is the newest moment each bot demonstrably acted — a review, a
	// completion reply, a classified event. A command with nothing after it was
	// never read, and the instruction is to remove only what has been.
	AnsweredAt map[string]time.Time
	// Live are the command IDs the current round still owns.
	Live map[int64]bool
	// Settled are commands belonging to the current head whose reviewer has
	// finished. Their round may still be waiting on another reviewer.
	Settled map[int64]bool
	// AdoptableFrom is the cutoff adoption itself uses: the head commit date,
	// raised to the last force-push. A command at or after it is still
	// adoptable, so removing it would make crq post a duplicate — unless the
	// round itself has already replaced it (Superseded). Zero when the head
	// commit could not be read, which is not permission to delete: the guard is
	// simply unevaluable, and an unevaluable guard keeps the comment.
	AdoptableFrom time.Time
	// Superseded are commands the round explicitly moved past by posting a newer
	// one. They are exempt from the head check: crq's own record that it has
	// replaced a command is stronger evidence than any timestamp.
	Superseded map[int64]bool
	// ReactionTargets are comments whose reaction is still completion evidence.
	// Deleting the comment would delete that evidence with it.
	ReactionTargets map[int64]bool
}

// StaleCommands returns trigger comments crq posted that have an answer and no
// remaining use as a live request or reaction target.
//
// Deleting a comment crq still reads is the way this becomes expensive. Three
// guards, and a command has to clear all of them:
//
//   - it belongs to no live round, or its reviewer has finished this head;
//   - the bot acted after it, so it was actually read rather than merely old;
//   - it predates the adoption cutoff, or its reviewer has finished this head.
//     An unanswered command newer than the cutoff may still be adopted and
//     must stay. An unreadable head keeps that guard in place.
//
// Only a command the round itself replaced (Superseded) skips the head check —
// crq's own record that it posted a successor outranks any timestamp.
//
// Anything crq did not post is not here at all: the caller only collects its own
// comments, so a human's "@coderabbitai review" is never a candidate.
func StaleCommands(in TidyInput) []int64 {
	var stale []int64
	for _, cmd := range in.Commands {
		if (in.Live[cmd.ID] && !in.Settled[cmd.ID]) || in.ReactionTargets[cmd.ID] {
			continue
		}
		answered, ok := in.AnsweredAt[cmd.Bot]
		if !ok || answered.Before(cmd.CreatedAt) {
			continue
		}
		if !in.Settled[cmd.ID] && !in.Superseded[cmd.ID] && (in.AdoptableFrom.IsZero() || !cmd.CreatedAt.Before(in.AdoptableFrom)) {
			continue
		}
		stale = append(stale, cmd.ID)
	}
	return stale
}
