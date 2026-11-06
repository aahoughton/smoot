package core

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Participant states.
const (
	StateActive    = "active"
	StateConcluded = "concluded"
	StateLeft      = "left"
	StateDropped   = "dropped"
)

// Presence values a participant can report.
const (
	PresenceWorking = "working"
	PresenceWaiting = "waiting"
	PresenceAway    = "away"
)

// Participant is an agent's membership in a conversation.
type Participant struct {
	Agent       string     `json:"agent"`
	State       string     `json:"state"`
	JoinedAt    time.Time  `json:"joined_at"`
	ReadSeq     int64      `json:"read_seq"`
	Presence    string     `json:"presence"`
	Note        string     `json:"note,omitempty"`
	CheckBackBy *time.Time `json:"check_back_by,omitempty"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	// Stale is true when an active or concluded participant has not called in
	// for longer than Options.StaleAfter.
	Stale bool `json:"stale"`
	// YourAccount is true when the caller's account joined under this name.
	YourAccount bool `json:"your_account"`

	account sql.NullInt64
}

// maxNoteBytes bounds a presence note.
const maxNoteBytes = 500

const participantColumns = `agent, state, joined_at, read_seq, presence, note, check_back_by, last_seen_at, account_id`

func (s *Service) scanParticipant(row interface{ Scan(...any) error }) (Participant, error) {
	var pt Participant
	var joined, seen int64
	var checkBack sql.NullInt64
	if err := row.Scan(&pt.Agent, &pt.State, &joined, &pt.ReadSeq, &pt.Presence, &pt.Note, &checkBack, &seen, &pt.account); err != nil {
		return Participant{}, err
	}
	pt.JoinedAt = fromMS(joined)
	pt.LastSeenAt = fromMS(seen)
	pt.CheckBackBy = fromNullMS(checkBack)
	inConversation := pt.State == StateActive || pt.State == StateConcluded
	pt.Stale = inConversation && s.now().Sub(pt.LastSeenAt) > s.opts.StaleAfter
	return pt, nil
}

func (s *Service) participants(ctx context.Context, q querier, convID int64) ([]Participant, error) {
	return collect(ctx, q, func(rows *sql.Rows) (Participant, error) { return s.scanParticipant(rows) },
		`SELECT `+participantColumns+` FROM participants WHERE conversation_id = ? ORDER BY joined_at, agent`, convID)
}

// participantsOf returns the participants of several conversations in one
// query, by conversation id.
func (s *Service) participantsOf(ctx context.Context, q querier, convIDs []int64) (map[int64][]Participant, error) {
	out := map[int64][]Participant{}
	if len(convIDs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(convIDs)), ",")
	args := make([]any, len(convIDs))
	for i, id := range convIDs {
		args[i] = id
	}
	type row struct {
		convID int64
		pt     Participant
	}
	rows, err := collect(ctx, q, func(rows *sql.Rows) (row, error) {
		var r row
		var err error
		r.pt, err = s.scanParticipant(prefixed{rows, &r.convID})
		return r, err
	}, `SELECT conversation_id, `+participantColumns+` FROM participants
		WHERE conversation_id IN (`+marks+`) ORDER BY joined_at, agent`, args...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.convID] = append(out[r.convID], r.pt)
	}
	return out, nil
}

// Observer is someone who has commented on a conversation without joining
// it.
type Observer struct {
	Name     string    `json:"name"`
	Comments int       `json:"comments"`
	LastAt   time.Time `json:"last_at"`
}

// observersOf returns the observers of several conversations in one query,
// by conversation id, each in order of first comment.
func (s *Service) observersOf(ctx context.Context, q querier, convIDs []int64) (map[int64][]Observer, error) {
	out := map[int64][]Observer{}
	if len(convIDs) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(convIDs)), ",")
	args := make([]any, len(convIDs))
	for i, id := range convIDs {
		args[i] = id
	}
	type row struct {
		convID int64
		ob     Observer
	}
	rows, err := collect(ctx, q, func(rows *sql.Rows) (row, error) {
		var r row
		var last int64
		err := rows.Scan(&r.convID, &r.ob.Name, &r.ob.Comments, &last)
		r.ob.LastAt = fromMS(last)
		return r, err
	}, `SELECT conversation_id, author, COUNT(*), MAX(created_at) FROM messages
		WHERE conversation_id IN (`+marks+`) AND kind = 'observer'
		GROUP BY conversation_id, author ORDER BY conversation_id, MIN(seq)`, args...)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.convID] = append(out[r.convID], r.ob)
	}
	return out, nil
}

// prefixed scans one leading column into first, then the rest as usual.
type prefixed struct {
	rows  *sql.Rows
	first any
}

func (p prefixed) Scan(dest ...any) error { return p.rows.Scan(append([]any{p.first}, dest...)...) }

// participant returns the agent's row, or nil if the agent never joined.
func (s *Service) participant(ctx context.Context, q querier, convID int64, agent string) (*Participant, error) {
	row := q.QueryRowContext(ctx, `SELECT `+participantColumns+`
		FROM participants WHERE conversation_id = ? AND agent = ?`, convID, agent)
	pt, err := s.scanParticipant(row)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pt, nil
}

// checkOwner refuses p acting as agent in c when the name belongs to
// another account. A name is bound to the account that first joined with
// it (or started the conversation with it), for the life of the
// conversation. Names bound under auth mode none (NULL) cannot be acted as
// by an account. The anonymous principal (auth mode none) is not checked.
func (s *Service) checkOwner(ctx context.Context, q querier, c *conv, p Principal, agent string) error {
	if p.IsAnonymous() {
		return nil
	}
	var owner sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT account_id FROM participants WHERE conversation_id = ? AND agent = ?`,
		c.id, agent).Scan(&owner)
	switch {
	case isNoRows(err):
		if agent != c.CreatedBy {
			return nil
		}
		owner = c.creatorAccount
	case err != nil:
		return err
	}
	switch {
	case !owner.Valid:
		return errorf(CodeForbidden, "%s in conversation %q was joined without an account; pick another name", agent, c.Name)
	case owner.Int64 != p.AccountID:
		return errorf(CodeForbidden, "%s in conversation %q belongs to another account; pick another name", agent, c.Name)
	}
	return nil
}

// act runs fn for p acting as agent in conversation name. In order, it
// authorizes p to participate in ns, takes a write from p's allowance,
// checks the agent's name, runs check (if any) for the caller's own
// validation, and settles a due timebox; then, in a write transaction, it
// loads the conversation and checks that p may act as agent there.
func (s *Service) act(ctx context.Context, p Principal, ns, name, agent string, check func() error, fn func(tx *wtx, c *conv) error) error {
	return s.actLimited(ctx, p, ns, name, agent, s.limitWrite, check, fn)
}

// visit is act for an agent's reads and waits, which draw on their own
// allowance.
func (s *Service) visit(ctx context.Context, p Principal, ns, name, agent string, check func() error, fn func(tx *wtx, c *conv) error) error {
	return s.actLimited(ctx, p, ns, name, agent, s.limitVisit, check, fn)
}

func (s *Service) actLimited(ctx context.Context, p Principal, ns, name, agent string, limit func(Principal) error, check func() error, fn func(tx *wtx, c *conv) error) error {
	if err := s.authorize(ctx, p, ns, ActionParticipate); err != nil {
		return err
	}
	if err := limit(p); err != nil {
		return err
	}
	if err := checkName("agent", agent); err != nil {
		return err
	}
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if err := s.settle(ctx, ns, name); err != nil {
		return err
	}
	return s.writeAs(ctx, p, ns, ActionParticipate, func(tx *wtx) error {
		c, err := s.loadConvTx(ctx, tx, ns, name)
		if err != nil {
			return err
		}
		if err := s.checkOwner(ctx, tx, c, p, agent); err != nil {
			return err
		}
		return fn(tx, c)
	})
}

// member returns the agent's row if they are active or concluded, and a
// not_participant error otherwise.
func (s *Service) member(ctx context.Context, q querier, c *conv, agent string) (*Participant, error) {
	pt, err := s.participant(ctx, q, c.id, agent)
	if err != nil {
		return nil, err
	}
	if pt == nil || (pt.State != StateActive && pt.State != StateConcluded) {
		return nil, errorf(CodeNotParticipant, "%s is not in conversation %q; join it first", agent, c.Name)
	}
	return pt, nil
}

func (s *Service) addParticipant(ctx context.Context, tx *wtx, c *conv, agent string) error {
	now := ms(s.now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO participants
		(conversation_id, agent, state, joined_at, last_seen_at, account_id) VALUES (?, ?, ?, ?, ?, ?)`,
		c.id, agent, StateActive, now, now, tx.account); err != nil {
		return err
	}
	_, err := s.appendMsg(ctx, tx, c, KindSystem, agent, agent+" joined", nil)
	return err
}

// presenceUnlessConcluded is the SQL value for a presence of working set as
// a side effect of a call, which a concluded participant does not take: its
// reads and waits are not work.
const presenceUnlessConcluded = `CASE WHEN state = 'concluded' THEN presence ELSE ? END`

func (s *Service) setState(ctx context.Context, tx *wtx, c *conv, agent, state, presence string) error {
	_, err := tx.ExecContext(ctx, `UPDATE participants SET state = ?, presence = ?, last_seen_at = ?
		WHERE conversation_id = ? AND agent = ?`, state, presence, ms(s.now()), c.id, agent)
	return err
}

// touch records that the agent called in and what it is doing. A concluded
// agent does not become working this way.
func (s *Service) touch(ctx context.Context, q querier, c *conv, agent, presence string) error {
	value := "?"
	if presence == PresenceWorking {
		value = presenceUnlessConcluded
	}
	_, err := q.ExecContext(ctx, `UPDATE participants SET presence = `+value+`, last_seen_at = ?
		WHERE conversation_id = ? AND agent = ?`, presence, ms(s.now()), c.id, agent)
	return err
}

func openOrError(c *conv) error {
	if c.Status != StatusOpen {
		return errorf(CodeConversationConcluded, "conversation %q has concluded", c.Name)
	}
	return nil
}

// JoinParams describe a join.
type JoinParams struct {
	Agent string
	// Resume must be true to rejoin under a name that is still active or
	// concluded, for an agent returning after a restart.
	Resume bool
}

// Join adds an agent to a conversation, or returns a left or dropped agent to
// it.
func (s *Service) Join(ctx context.Context, p Principal, ns, name string, jp JoinParams) (Standing, error) {
	var st Standing
	err := s.act(ctx, p, ns, name, jp.Agent, nil, func(tx *wtx, c *conv) error {
		if err := openOrError(c); err != nil {
			return err
		}
		pt, err := s.participant(ctx, tx, c.id, jp.Agent)
		if err != nil {
			return err
		}
		switch {
		case pt == nil:
			if err := s.addParticipant(ctx, tx, c, jp.Agent); err != nil {
				return err
			}
		case pt.State == StateActive || pt.State == StateConcluded:
			if !jp.Resume {
				return errorf(CodeNameActive, "%s is already in conversation %q; pass resume=true if you are %s returning, otherwise pick another name", jp.Agent, name, jp.Agent)
			}
			if err := s.touch(ctx, tx, c, jp.Agent, PresenceWorking); err != nil {
				return err
			}
			if _, err := s.appendMsg(ctx, tx, c, KindSystem, jp.Agent, jp.Agent+" resumed", nil); err != nil {
				return err
			}
		default:
			if err := s.setState(ctx, tx, c, jp.Agent, StateActive, PresenceWorking); err != nil {
				return err
			}
			if _, err := s.appendMsg(ctx, tx, c, KindSystem, jp.Agent, jp.Agent+" rejoined", nil); err != nil {
				return err
			}
		}
		st, err = s.standing(ctx, tx, c, jp.Agent)
		return err
	})
	return st, err
}

// Live reports whether pt is in its conversation and has either called in
// lately or said when it will be back.
func (s *Service) Live(pt Participant) bool {
	if pt.State != StateActive && pt.State != StateConcluded {
		return false
	}
	return !pt.Stale || (pt.CheckBackBy != nil && s.now().Before(*pt.CheckBackBy))
}

// LeaveParams describe a leave.
type LeaveParams struct {
	Agent string
	// Body, if set, is posted as a final message before leaving.
	Body string
}

// Leave removes an agent from a conversation. Leaving while expected (the
// conversation is open, has an end condition, and the agent has not
// concluded) is recorded as dropped, except for the creator, who may step
// away. Leaving a concluded conversation changes nothing and returns the
// standing; a final message with it is rejected.
func (s *Service) Leave(ctx context.Context, p Principal, ns, name string, lp LeaveParams) (Standing, error) {
	var st Standing
	check := func() error { return s.checkBody(lp.Body, true) }
	err := s.act(ctx, p, ns, name, lp.Agent, check, func(tx *wtx, c *conv) error {
		pt, err := s.member(ctx, tx, c, lp.Agent)
		if err != nil {
			return err
		}
		if c.Status != StatusOpen {
			// The log is closed and nobody is expected: nothing to record.
			if lp.Body != "" {
				return openOrError(c)
			}
			st, err = s.standing(ctx, tx, c, lp.Agent)
			return err
		}
		if lp.Body != "" {
			// A final message is a post like any other, so it withdraws a
			// conclusion first.
			if pt.State == StateConcluded {
				if _, err := s.appendMsg(ctx, tx, c, KindSystem, lp.Agent, lp.Agent+" withdrew their conclusion", nil); err != nil {
					return err
				}
				pt.State = StateActive
			}
			if _, err := s.appendPost(ctx, tx, c, KindSay, lp.Agent, lp.Body, nil, &pt.ReadSeq); err != nil {
				return err
			}
		}
		state, note := StateLeft, " left"
		if expected(c, pt) && lp.Agent != c.CreatedBy {
			state, note = StateDropped, " dropped out before the conversation concluded"
		}
		if err := s.setState(ctx, tx, c, lp.Agent, state, PresenceAway); err != nil {
			return err
		}
		if _, err := s.appendMsg(ctx, tx, c, KindSystem, lp.Agent, lp.Agent+note, nil); err != nil {
			return err
		}
		if err := s.checkAgreement(ctx, tx, c); err != nil {
			return err
		}
		st, err = s.standing(ctx, tx, c, lp.Agent)
		return err
	})
	return st, err
}

// expected reports whether the conversation is waiting on this participant.
func expected(c *conv, pt *Participant) bool {
	return c.Status == StatusOpen && c.End != EndNone && pt != nil && pt.State == StateActive
}

// checkAgreement settles an agreement conversation once nobody is active
// and at least one participant has concluded. When every conclusion
// agrees to the same message or commit, or every one is BLOCKED, the
// conversation concludes; otherwise a system line says how they differ
// and the concluded participants are active again, so it waits on them.
func (s *Service) checkAgreement(ctx context.Context, tx *wtx, c *conv) error {
	if c.Status != StatusOpen || c.End != EndAgreement {
		return nil
	}
	var active, concluded int
	err := tx.QueryRowContext(ctx, `SELECT
		coalesce(sum(state = 'active'), 0), coalesce(sum(state = 'concluded'), 0)
		FROM participants WHERE conversation_id = ?`, c.id).Scan(&active, &concluded)
	if err != nil {
		return err
	}
	if active > 0 || concluded == 0 {
		return nil
	}
	ps, err := s.positions(ctx, tx, c)
	if err != nil {
		return err
	}
	if ref, ok := agreedRef(ps); ok {
		return s.concludeConv(ctx, tx, c, "every remaining participant agreed to "+ref)
	}
	if allBlocked(ps) {
		return s.concludeConv(ctx, tx, c, "every remaining participant blocked")
	}
	if _, err := s.appendMsg(ctx, tx, c, KindSystem, SystemAuthor, differ(ps), nil); err != nil {
		return err
	}
	for _, p := range ps {
		if err := s.setState(ctx, tx, c, p.agent, StateActive, PresenceWorking); err != nil {
			return err
		}
	}
	return nil
}

// PresenceParams update an agent's presence.
type PresenceParams struct {
	Agent       string
	Presence    string
	Note        string
	CheckBackBy *time.Time
}

// SetPresence records what an agent is doing and when it will check back.
func (s *Service) SetPresence(ctx context.Context, p Principal, ns, name string, pp PresenceParams) (Participant, error) {
	var out Participant
	check := func() error {
		switch pp.Presence {
		case PresenceWorking, PresenceWaiting, PresenceAway:
		default:
			return errorf(CodeInvalid, "invalid presence %q: want working, waiting or away", pp.Presence)
		}
		if len(pp.Note) > maxNoteBytes {
			return errorf(CodeInvalid, "note is longer than %d bytes", maxNoteBytes)
		}
		return nil
	}
	err := s.act(ctx, p, ns, name, pp.Agent, check, func(tx *wtx, c *conv) error {
		if _, err := s.member(ctx, tx, c, pp.Agent); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE participants
			SET presence = ?, note = ?, check_back_by = ?, last_seen_at = ?
			WHERE conversation_id = ? AND agent = ?`,
			pp.Presence, pp.Note, nullMS(pp.CheckBackBy), ms(s.now()), c.id, pp.Agent); err != nil {
			return err
		}
		pt, err := s.participant(ctx, tx, c.id, pp.Agent)
		if err != nil {
			return err
		}
		out = *pt
		return nil
	})
	out.YourAccount = p.owns(out.account)
	return out, err
}

// ConcludeParams record an agent's final position.
type ConcludeParams struct {
	Agent string
	Body  string
}

// Conclude records the agent's final position and marks them concluded.
// Posting again later withdraws the conclusion.
func (s *Service) Conclude(ctx context.Context, p Principal, ns, name string, cp ConcludeParams) (Standing, error) {
	var st Standing
	check := func() error { return s.checkBody(cp.Body, false) }
	err := s.act(ctx, p, ns, name, cp.Agent, check, func(tx *wtx, c *conv) error {
		if err := openOrError(c); err != nil {
			return err
		}
		pt, err := s.member(ctx, tx, c, cp.Agent)
		if err != nil {
			return err
		}
		if c.End == EndAgreement {
			if _, err := parsePosition(cp.Body); err != nil {
				return err
			}
		}
		if _, err := s.appendPost(ctx, tx, c, KindConclude, cp.Agent, cp.Body, nil, &pt.ReadSeq); err != nil {
			return err
		}
		if err := s.setState(ctx, tx, c, cp.Agent, StateConcluded, PresenceWaiting); err != nil {
			return err
		}
		// A note describes work in progress, which concluding ends.
		if _, err := tx.ExecContext(ctx, `UPDATE participants SET note = '', check_back_by = NULL
			WHERE conversation_id = ? AND agent = ?`, c.id, cp.Agent); err != nil {
			return err
		}
		if err := s.checkAgreement(ctx, tx, c); err != nil {
			return err
		}
		st, err = s.standing(ctx, tx, c, cp.Agent)
		return err
	})
	return st, err
}

// CloseParams close a conversation.
type CloseParams struct {
	Agent string
	// Body, if set, is recorded as the creator's conclusion.
	Body string
}

// Close concludes a conversation. Only its creator may close it.
func (s *Service) Close(ctx context.Context, p Principal, ns, name string, cp CloseParams) (Standing, error) {
	var st Standing
	check := func() error { return s.checkBody(cp.Body, true) }
	err := s.act(ctx, p, ns, name, cp.Agent, check, func(tx *wtx, c *conv) error {
		if err := openOrError(c); err != nil {
			return err
		}
		if cp.Agent != c.CreatedBy {
			return errorf(CodeNotCoordinator, "only %s, who started conversation %q, can close it", c.CreatedBy, name)
		}
		if cp.Body != "" {
			// A coordinator who never joined has no read position.
			var seen *int64
			if err := tx.QueryRowContext(ctx, `SELECT read_seq FROM participants WHERE conversation_id = ? AND agent = ?`,
				c.id, cp.Agent).Scan(&seen); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := s.appendPost(ctx, tx, c, KindConclude, cp.Agent, cp.Body, nil, seen); err != nil {
				return err
			}
		}
		if err := s.concludeConv(ctx, tx, c, "closed by "+cp.Agent); err != nil {
			return err
		}
		var err error
		st, err = s.standing(ctx, tx, c, cp.Agent)
		return err
	})
	return st, err
}

// owns reports whether p is the account recorded on a row. The anonymous
// principal owns nothing, and neither does anyone a row without an account
// (auth mode none, imported data).
func (p Principal) owns(account sql.NullInt64) bool {
	return !p.IsAnonymous() && account.Valid && account.Int64 == p.AccountID
}

// The mark functions set YourAccount for the caller on what a public method
// returns. A path that skips them leaves it false, which is the safe answer.

// System messages are the server's, whoever's request led to them.

func markMessages(p Principal, ms []Message) {
	for i := range ms {
		ms[i].YourAccount = ms[i].Kind != KindSystem && p.owns(ms[i].account)
	}
}

func markPreviews(p Principal, ps []Preview) {
	for i := range ps {
		ps[i].YourAccount = ps[i].Kind != KindSystem && p.owns(ps[i].account)
	}
}

func markParticipants(p Principal, c *Conversation) {
	for i := range c.Participants {
		c.Participants[i].YourAccount = p.owns(c.Participants[i].account)
	}
}
