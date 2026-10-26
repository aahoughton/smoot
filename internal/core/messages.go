package core

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

func (s *Service) checkBody(body string, allowEmpty bool) error {
	if !allowEmpty && strings.TrimSpace(body) == "" {
		return errorf(CodeInvalid, "body is empty")
	}
	if len(body) > s.opts.MaxBodyBytes {
		return errorf(CodeMessageTooLarge, "body is %d bytes; the limit is %d", len(body), s.opts.MaxBodyBytes)
	}
	return nil
}

// SayParams describe a message from a participant.
type SayParams struct {
	Agent string
	Body  string
	// ReplyTo, if set, names the seq this message answers.
	ReplyTo *int64
	// ExpectedLastSeq, if set, rejects the post with CodeStaleContext when
	// another message has landed since.
	ExpectedLastSeq *int64
	// IdempotencyKey, if set, makes a retry safe: a post repeating a key
	// already used in the conversation returns that message instead of
	// posting again. At most 128 bytes.
	IdempotencyKey string
	// InlineBytes, if positive, returns the messages the agent had not read
	// in full when their bodies total at most this many bytes, and marks
	// them read. Otherwise they come back as previews and stay unread.
	InlineBytes int
}

// SayResult is the appended message and the agent's standing after it.
// Missed is set when others had posted messages the agent had not read.
type SayResult struct {
	Message  Message  `json:"message"`
	Missed   *Missed  `json:"missed,omitempty"`
	Standing Standing `json:"standing"`
}

// Missed summarizes the messages from others that the agent had not read
// when it posted: in full (Messages, now marked read) when they were short,
// otherwise as Previews of the latest.
type Missed struct {
	New      int           `json:"new"`
	Authors  []AuthorCount `json:"authors"`
	Previews []Preview     `json:"previews,omitempty"`
	Messages []Message     `json:"messages,omitempty"`
}

// Say appends a message from a participant. A concluded participant who says
// something withdraws their conclusion and becomes active again.
func (s *Service) Say(ctx context.Context, p Principal, ns, name string, sp SayParams) (SayResult, error) {
	var out SayResult
	check := func() error {
		if err := s.checkBody(sp.Body, false); err != nil {
			return err
		}
		if len(sp.IdempotencyKey) > maxIdempotencyKey {
			return errorf(CodeInvalid, "idempotency_key is longer than %d bytes", maxIdempotencyKey)
		}
		return nil
	}
	err := s.act(ctx, p, ns, name, sp.Agent, check, func(tx *wtx, c *conv) error {
		// A retry of a post that went through returns it, before any check
		// that the first post itself would now fail (expected_last_seq).
		if sp.IdempotencyKey != "" {
			prior, err := s.messageByKey(ctx, tx, c, sp.IdempotencyKey)
			if err != nil {
				return err
			}
			if prior != nil {
				if prior.Author != sp.Agent || prior.Kind != KindSay || prior.Body != sp.Body || !sameReply(prior.ReplyTo, sp.ReplyTo) {
					return errorf(CodeIdempotencyConflict, "idempotency_key %q was used for a different message (#%d)", sp.IdempotencyKey, prior.Seq)
				}
				out.Message = *prior
				out.Standing, err = s.standing(ctx, tx, c, sp.Agent)
				return err
			}
		}
		if err := openOrError(c); err != nil {
			return err
		}
		pt, err := s.member(ctx, tx, c, sp.Agent)
		if err != nil {
			return err
		}
		if sp.ExpectedLastSeq != nil && *sp.ExpectedLastSeq != c.LastSeq {
			e := errorf(CodeStaleContext, "conversation %q has moved to seq %d since %d; read the new messages and decide whether to still post", name, c.LastSeq, *sp.ExpectedLastSeq)
			e.LastSeq = c.LastSeq
			return e
		}
		if sp.ReplyTo != nil && (*sp.ReplyTo < 1 || *sp.ReplyTo > c.LastSeq) {
			return errorf(CodeInvalid, "reply_to %d is not a message in conversation %q", *sp.ReplyTo, name)
		}
		out.Missed, err = s.missed(ctx, tx, c, pt.ReadSeq, sp.Agent, sp.InlineBytes)
		if err != nil {
			return err
		}
		if pt.State == StateConcluded {
			if _, err := s.appendMsg(ctx, tx, c, KindSystem, sp.Agent, sp.Agent+" withdrew their conclusion", nil); err != nil {
				return err
			}
		}
		m, err := s.appendPost(ctx, tx, c, KindSay, sp.Agent, sp.Body, sp.ReplyTo, &pt.ReadSeq)
		if err != nil {
			return err
		}
		if sp.IdempotencyKey != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET idempotency_key = ? WHERE conversation_id = ? AND seq = ?`,
				sp.IdempotencyKey, c.id, m.Seq); err != nil {
				return err
			}
		}
		if err := s.setState(ctx, tx, c, sp.Agent, StateActive, PresenceWorking); err != nil {
			return err
		}
		out.Message = m
		out.Standing, err = s.standing(ctx, tx, c, sp.Agent)
		return err
	})
	out.Message.YourAccount = p.owns(out.Message.account)
	if out.Missed != nil {
		markMessages(p, out.Missed.Messages)
		markPreviews(p, out.Missed.Previews)
	}
	return out, err
}

// missed summarizes messages from others in (after, c.LastSeq], or returns
// nil if there are none. When they fit inlineBytes it returns them in full
// and moves the agent's read position past them.
func (s *Service) missed(ctx context.Context, tx *wtx, c *conv, after int64, agent string, inlineBytes int) (*Missed, error) {
	if after >= c.LastSeq {
		return nil, nil
	}
	authors, err := collect(ctx, tx, func(rows *sql.Rows) (AuthorCount, error) {
		var ac AuthorCount
		err := rows.Scan(&ac.Author, &ac.Count)
		return ac, err
	}, `SELECT author, count(*) FROM messages
		WHERE conversation_id = ? AND seq > ? AND seq <= ? AND (author != ? OR kind = 'observer')
		GROUP BY author ORDER BY min(seq)`, c.id, after, c.LastSeq, agent)
	if err != nil || len(authors) == 0 {
		return nil, err
	}
	m := &Missed{Authors: authors}
	for _, ac := range authors {
		m.New += ac.Count
	}
	if inlineBytes > 0 && m.New <= maxInlineMessages {
		res, covered, err := s.page(ctx, tx, c, after, maxInlineMessages, agent)
		if err != nil {
			return nil, err
		}
		size := 0
		for _, msg := range res.Messages {
			size += len(msg.Body)
		}
		if !res.More && size <= inlineBytes {
			if _, err := tx.ExecContext(ctx, `UPDATE participants SET read_seq = max(read_seq, ?)
				WHERE conversation_id = ? AND agent = ?`, covered, c.id, agent); err != nil {
				return nil, err
			}
			m.Messages = res.Messages
			return m, nil
		}
	}
	m.Previews, err = s.previews(ctx, tx, c, after, agent)
	return m, err
}

// maxIdempotencyKey bounds a post's idempotency key.
const maxIdempotencyKey = 128

// messageByKey returns the message posted with key in c, or nil.
func (s *Service) messageByKey(ctx context.Context, q querier, c *conv, key string) (*Message, error) {
	var m Message
	var created int64
	err := q.QueryRowContext(ctx, `SELECT seq, kind, author, body, reply_to, seen_seq, created_at, account_id FROM messages
		WHERE conversation_id = ? AND idempotency_key = ?`, c.id, key).Scan(&m.Seq, &m.Kind, &m.Author, &m.Body, &m.ReplyTo, &m.SeenSeq, &created, &m.account)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = fromMS(created)
	return &m, nil
}

// ObserveParams describe a comment from someone watching, not participating.
type ObserveParams struct {
	Name string
	Body string
}

// Observe appends an observer comment. Observers never join, hold no
// presence, and do not count toward any end condition.
func (s *Service) Observe(ctx context.Context, p Principal, ns, name string, op ObserveParams) (Message, error) {
	if err := s.authorize(ctx, p, ns, ActionObserve); err != nil {
		return Message{}, err
	}
	if err := s.limitWrite(p); err != nil {
		return Message{}, err
	}
	if !p.IsAnonymous() {
		// An account comments under its own name, whatever it asked for.
		a, err := s.accountByID(ctx, s.db, p.AccountID)
		if err != nil {
			return Message{}, err
		}
		op.Name = a.Name
	}
	if err := checkName("observer", op.Name); err != nil {
		return Message{}, err
	}
	if err := s.checkBody(op.Body, false); err != nil {
		return Message{}, err
	}
	var out Message
	if err := s.settle(ctx, ns, name); err != nil {
		return Message{}, err
	}
	err := s.writeAs(ctx, p, ns, ActionObserve, func(tx *wtx) error {
		c, err := s.loadConvTx(ctx, tx, ns, name)
		if err != nil {
			return err
		}
		if err := openOrError(c); err != nil {
			return err
		}
		if p.IsAnonymous() {
			// Only auth mode none lets an observer pick its name.
			pt, err := s.participant(ctx, tx, c.id, op.Name)
			if err != nil {
				return err
			}
			if pt != nil {
				return errorf(CodeNameActive, "%q is a participant in this conversation; comment under another name", op.Name)
			}
		}
		out, err = s.appendMsg(ctx, tx, c, KindObserver, op.Name, op.Body, nil)
		return err
	})
	out.YourAccount = p.owns(out.account)
	return out, err
}

// Read limits.
const (
	DefaultReadLimit = 50
	MaxReadLimit     = 500
)

// ReadParams select messages to read.
type ReadParams struct {
	// Agent, if set, must be a participant. The read starts after their
	// read_seq unless After is set, and advances read_seq past what it
	// covers. Without After, the agent's own messages are skipped: it wrote
	// them. With After (re-reading, or checking whether a post landed), they
	// are included.
	Agent string
	After *int64
	Limit int
	// Tail, if set, reads only the last Tail messages and, for an agent,
	// marks everything before them read too: a late joiner skips the middle
	// on purpose. It cannot be combined with After.
	Tail int
	// MaxBytes, if set, ends the page early once message bodies total more
	// than this; the page always holds at least one message, and an
	// agent's read position moves only past what was returned.
	MaxBytes int
}

// ReadResult is a page of the log.
type ReadResult struct {
	Messages []Message `json:"messages"`
	LastSeq  int64     `json:"last_seq"`
	// More is true when messages past the last one returned exist.
	More     bool      `json:"more"`
	Standing *Standing `json:"standing,omitempty"`
}

// Read returns messages after a seq, oldest first.
func (s *Service) Read(ctx context.Context, p Principal, ns, name string, rp ReadParams) (ReadResult, error) {
	// Reading as an agent moves its read position, so it takes the right to
	// participate; reading without one is a plain read.
	if rp.Agent != "" {
		return s.readAs(ctx, p, ns, name, rp)
	}
	if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
		return ReadResult{}, err
	}
	if err := rp.check(); err != nil {
		return ReadResult{}, err
	}
	c, err := s.loadConvRead(ctx, ns, name)
	if err != nil {
		return ReadResult{}, err
	}
	var after int64
	if rp.After != nil {
		after = *rp.After
	}
	if rp.Tail > 0 {
		tailAfter, err := s.tailStart(ctx, s.db, c, rp.Tail, "")
		if err != nil {
			return ReadResult{}, err
		}
		after = max(after, tailAfter)
	}
	out, _, err := s.page(ctx, s.db, c, after, rp.Limit, "")
	out, _ = capBytes(out, 0, rp.MaxBytes)
	markMessages(p, out.Messages)
	return out, err
}

// check validates rp and fills in its default limit.
func (rp *ReadParams) check() error {
	switch {
	case rp.Limit == 0:
		rp.Limit = DefaultReadLimit
	case rp.Limit < 0 || rp.Limit > MaxReadLimit:
		return errorf(CodeInvalid, "limit must be between 1 and %d", MaxReadLimit)
	}
	if rp.After != nil && *rp.After < 0 {
		return errorf(CodeInvalid, "after must not be negative")
	}
	switch {
	case rp.Tail < 0 || rp.Tail > MaxReadLimit:
		return errorf(CodeInvalid, "tail must be between 1 and %d", MaxReadLimit)
	case rp.Tail > 0 && rp.After != nil:
		return errorf(CodeInvalid, "use tail or after, not both")
	case rp.Tail > 0:
		rp.Limit = rp.Tail
	}
	return nil
}

// readAs is Read as an agent, which moves its read position.
func (s *Service) readAs(ctx context.Context, p Principal, ns, name string, rp ReadParams) (ReadResult, error) {
	var out ReadResult
	err := s.visit(ctx, p, ns, name, rp.Agent, rp.check, func(tx *wtx, c *conv) error {
		pt, err := s.member(ctx, tx, c, rp.Agent)
		if err != nil {
			return err
		}
		after, skip := pt.ReadSeq, rp.Agent
		if rp.After != nil {
			if *rp.After > c.LastSeq {
				return errorf(CodeInvalid, "after %d is past the last message (#%d)", *rp.After, c.LastSeq)
			}
			after, skip = *rp.After, ""
		}
		if rp.Tail > 0 {
			tailAfter, err := s.tailStart(ctx, tx, c, rp.Tail, skip)
			if err != nil {
				return err
			}
			after = max(after, tailAfter)
		}
		var covered int64
		out, covered, err = s.page(ctx, tx, c, after, rp.Limit, skip)
		if err != nil {
			return err
		}
		out, covered = capBytes(out, covered, rp.MaxBytes)
		if _, err := tx.ExecContext(ctx, `UPDATE participants SET read_seq = max(read_seq, ?)
			WHERE conversation_id = ? AND agent = ?`, covered, c.id, rp.Agent); err != nil {
			return err
		}
		if err := s.touch(ctx, tx, c, rp.Agent, PresenceWorking); err != nil {
			return err
		}
		st, err := s.standing(ctx, tx, c, rp.Agent)
		out.Standing = &st
		return err
	})
	markMessages(p, out.Messages)
	return out, err
}

// tailStart returns the seq to read after so that a page holds the last n
// messages not by skipAuthor.
func (s *Service) tailStart(ctx context.Context, q querier, c *conv, n int, skipAuthor string) (int64, error) {
	var first sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT min(seq) FROM (SELECT seq FROM messages
		WHERE conversation_id = ? AND seq <= ? AND (author != ? OR kind = 'observer') ORDER BY seq DESC LIMIT ?)`,
		c.id, c.LastSeq, skipAuthor, n).Scan(&first)
	if err != nil || !first.Valid {
		return 0, err
	}
	return first.Int64 - 1, nil
}

// page returns up to limit messages in (after, c.LastSeq], skipping those
// by skipAuthor if it is set. covered is the highest seq the page accounts
// for: the last message returned when more follow, otherwise c.LastSeq.
func (s *Service) page(ctx context.Context, q querier, c *conv, after int64, limit int, skipAuthor string) (ReadResult, int64, error) {
	msgs, err := s.messages(ctx, q, c.id, after, c.LastSeq, limit+1, skipAuthor)
	if err != nil {
		return ReadResult{}, 0, err
	}
	out := ReadResult{Messages: msgs, LastSeq: c.LastSeq}
	covered := max(c.LastSeq, after)
	if len(msgs) > limit {
		out.Messages, out.More = msgs[:limit], true
		covered = msgs[limit-1].Seq
	}
	return out, covered, nil
}

// sameReply reports whether two optional reply targets are the same.
func sameReply(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// capBytes ends a page before the message that takes its bodies past
// maxBytes, keeping at least one; covered moves back to the last message
// kept. maxBytes 0 keeps the page as it is.
func capBytes(out ReadResult, covered int64, maxBytes int) (ReadResult, int64) {
	if maxBytes <= 0 {
		return out, covered
	}
	total := 0
	for i, m := range out.Messages {
		total += len(m.Body)
		if total > maxBytes && i > 0 {
			out.Messages, out.More = out.Messages[:i], true
			return out, out.Messages[i-1].Seq
		}
	}
	return out, covered
}

// messages returns up to limit messages with after < seq <= upto, skipping
// those by skipAuthor if it is set. Callers pass the last_seq they loaded, so
// a page never mixes in later appends.
func (s *Service) messages(ctx context.Context, q querier, convID, after, upto int64, limit int, skipAuthor string) ([]Message, error) {
	ms, err := collect(ctx, q, func(rows *sql.Rows) (Message, error) {
		var m Message
		var created int64
		err := rows.Scan(&m.Seq, &m.Kind, &m.Author, &m.Body, &m.ReplyTo, &m.SeenSeq, &created, &m.account)
		m.CreatedAt = fromMS(created)
		return m, err
	}, `SELECT seq, kind, author, body, reply_to, seen_seq, created_at, account_id
		FROM messages WHERE conversation_id = ? AND seq > ? AND seq <= ? AND (author != ? OR kind = 'observer') ORDER BY seq LIMIT ?`,
		convID, after, upto, skipAuthor, limit)
	if err != nil {
		return nil, err
	}
	return ms, withAttachments(ctx, q, convID, ms)
}

// FollowLimit bounds the messages one Follow call returns.
const FollowLimit = 200

// Follow blocks until the conversation's log moves past after, or timeout
// passes, then returns the new messages (all authors). It is for viewers:
// it needs no participant, sets no presence, and moves no read position.
func (s *Service) Follow(ctx context.Context, p Principal, ns, name string, after int64, timeout time.Duration) (ReadResult, error) {
	return s.follow(ctx, p, ns, name, after, timeout, true)
}

// Viewer is a viewer slot held open across many follows, for a stream that
// stays connected: it counts against the conversation's viewer cap until
// Close, including while its owner is busy writing to a slow client.
type Viewer struct {
	s        *Service
	p        Principal
	ns, name string
	convID   int64
	once     sync.Once
}

// OpenViewer reserves a viewer slot on a conversation.
func (s *Service) OpenViewer(ctx context.Context, p Principal, ns, name string) (*Viewer, error) {
	if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
		return nil, err
	}
	c, err := s.loadConvRead(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	if err := s.watch(c, p); err != nil {
		return nil, err
	}
	return &Viewer{s: s, p: p, ns: ns, name: name, convID: c.id}, nil
}

// Follow is Service.Follow within the viewer's slot.
func (v *Viewer) Follow(ctx context.Context, after int64, timeout time.Duration) (ReadResult, error) {
	return v.s.follow(ctx, v.p, v.ns, v.name, after, timeout, false)
}

// Close releases the viewer's slot.
func (v *Viewer) Close() { v.once.Do(func() { v.s.hub.unwatch(v.convID, v.p) }) }

// watch takes a viewer slot on c for p.
func (s *Service) watch(c *conv, p Principal) error {
	switch s.hub.watch(c.id, p, slots{s.opts.MaxWatchers, s.opts.MaxPerAccount}) {
	case fullConversation:
		return errorf(CodeTooManyWatchers, "conversation %q already has %d viewers; retry later", c.Name, s.opts.MaxWatchers)
	case fullAccount:
		return errorf(CodeTooManyWatchers, "your account already has %d viewers on conversation %q; retry later", s.opts.MaxPerAccount, c.Name)
	}
	return nil
}

func (s *Service) follow(ctx context.Context, p Principal, ns, name string, after int64, timeout time.Duration, takeSlot bool) (ReadResult, error) {
	if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
		return ReadResult{}, err
	}
	if after < 0 || timeout < 0 {
		return ReadResult{}, errorf(CodeInvalid, "after and timeout must not be negative")
	}
	c, err := s.loadConvRead(ctx, ns, name)
	if err != nil {
		return ReadResult{}, err
	}
	if takeSlot {
		if err := s.watch(c, p); err != nil {
			return ReadResult{}, err
		}
		defer s.hub.unwatch(c.id, p)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	recheck := time.NewTicker(s.opts.RecheckEvery)
	defer recheck.Stop()
	timedOut := false
	for {
		ch := s.hub.channel(c.id)
		// loadConvRead records a due timebox expiry, which then shows up
		// as a new message.
		c, err = s.loadConvRead(ctx, ns, name)
		if err != nil {
			return ReadResult{}, err
		}
		if c.LastSeq > after {
			// The caller may have lost access while blocked.
			if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
				return ReadResult{}, err
			}
			out, _, err := s.page(ctx, s.db, c, after, FollowLimit, "")
			markMessages(p, out.Messages)
			return out, err
		}
		if timedOut {
			if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
				return ReadResult{}, err
			}
			return ReadResult{Messages: []Message{}, LastSeq: c.LastSeq}, nil
		}
		// Wake at a pending timebox deadline so its expiry is reported.
		var expiry <-chan time.Time
		var expiryTimer *time.Timer
		if c.Status == StatusOpen && c.End == EndTimebox && c.EndsAt != nil {
			expiryTimer = time.NewTimer(max(c.EndsAt.Sub(s.now()), 0))
			expiry = expiryTimer.C
		}
		select {
		case <-ch:
		case <-expiry:
		case <-recheck.C:
			if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
				return ReadResult{}, err
			}
		case <-deadline.C:
			timedOut = true
		case <-ctx.Done():
		}
		if expiryTimer != nil {
			expiryTimer.Stop()
		}
		if ctx.Err() != nil {
			return ReadResult{}, ctx.Err()
		}
	}
}
