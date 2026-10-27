package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxAttachmentBytes caps one attached file.
const MaxAttachmentBytes = 10 << 20

// SHA256Header is the HTTP header that carries a downloaded attachment's
// sha256, hex encoded.
const SHA256Header = "X-Smoot-Sha256"

// concludedAt is the time conversation c (an alias in the query) ended, or
// NULL while it is open. An elapsed timebox ended at its deadline, or at
// its close if that came first, whenever its conclusion was recorded: a
// read after the deadline records it late and must not extend retention.
const concludedAt = `(CASE
	WHEN c.end_condition = 'timebox' AND c.ends_at <= ?1 THEN MIN(c.ends_at, COALESCE(c.concluded_at, c.ends_at))
	WHEN c.status = 'concluded' THEN c.concluded_at END)`

// MaxFileNameBytes caps an attachment's file name.
const MaxFileNameBytes = 255

// Defaults for the attachment Options left zero.
const (
	DefaultAttachmentCap    = 1 << 30
	DefaultAttachmentQuota  = 100 << 20
	DefaultAttachmentExpiry = 30 * 24 * time.Hour
)

// Why an attachment's bytes are no longer stored.
const (
	// GoneExpired: the conversation concluded longer ago than
	// Options.AttachmentExpiry.
	GoneExpired = "expired"
	// GoneEvicted: removed to make room under Options.AttachmentCap.
	GoneEvicted = "evicted"
	// GoneDeleted: a server admin deleted it.
	GoneDeleted = "deleted"
	// GoneMissing: loaded from a dump, which never holds the bytes.
	GoneMissing = "missing"
)

// Attachment describes the file an attachment message carries.
type Attachment struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Gone is why the bytes are no longer stored (GoneExpired, ...);
	// empty while they can be downloaded.
	Gone string `json:"gone,omitempty"`
}

// FormatSize writes a byte count for people: 512 B, 1.5 KiB, 2.0 MiB.
func FormatSize(n int64) string {
	switch {
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KiB"
	default:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
	}
}

// AttachmentsEnabled reports whether participants may attach files
// (Options.Attachments).
func (s *Service) AttachmentsEnabled() bool { return s.opts.Attachments }

func (s *Service) requireAttachments() error {
	if !s.opts.Attachments {
		return errorf(CodeForbidden, "attachments are off on this server")
	}
	return nil
}

// CheckFileName reports whether name may name an attachment: 1 to
// MaxFileNameBytes bytes of UTF-8, not "." or "..", with no path
// separator, line or paragraph separator, control character or
// bidirectional control (which could make a name display as another, or
// as more than one line). Clients check it again before writing a
// downloaded file.
func CheckFileName(name string) error {
	switch {
	case name == "":
		return errorf(CodeInvalid, "file name is empty")
	case len(name) > MaxFileNameBytes:
		return errorf(CodeInvalid, "file name is %d bytes; the limit is %d", len(name), MaxFileNameBytes)
	case !utf8.ValidString(name):
		return errorf(CodeInvalid, "file name is not valid UTF-8")
	case name == "." || name == "..":
		return errorf(CodeInvalid, "file name %+q is not allowed", name)
	case strings.ContainsAny(name, `/\`):
		return errorf(CodeInvalid, "file name %+q contains a path separator", name)
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.In(r, unicode.Zl, unicode.Zp) }):
		return errorf(CodeInvalid, "file name %+q contains a line separator", name)
	case strings.ContainsFunc(name, unicode.IsControl):
		return errorf(CodeInvalid, "file name %+q contains a control character", name)
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.Is(unicode.Bidi_Control, r) }):
		return errorf(CodeInvalid, "file name %+q contains a bidirectional control", name)
	}
	return nil
}

// AttachParams describe a file a participant attaches.
type AttachParams struct {
	Agent string
	Name  string
	Data  []byte
}

// AttachResult is the attachment message and the agent's standing after it.
type AttachResult struct {
	Message  Message  `json:"message"`
	Standing Standing `json:"standing"`
}

// Attach stores a file in an open conversation and appends an attachment
// message for it. The agent must be a participant; attaching does not
// change its state, so a concluded agent stays concluded. Bytes already
// stored in the conversation are not stored again or counted again. New
// bytes must fit Options.AttachmentQuota for the conversation, and
// Options.AttachmentCap for the server once bytes from concluded
// conversations are evicted, least recently uploaded first; otherwise
// nothing is stored or evicted.
func (s *Service) Attach(ctx context.Context, p Principal, ns, name string, ap AttachParams) (AttachResult, error) {
	if err := s.requireAttachments(); err != nil {
		return AttachResult{}, err
	}
	check := func() error {
		if err := CheckFileName(ap.Name); err != nil {
			return err
		}
		if len(ap.Data) > MaxAttachmentBytes {
			return errorf(CodeAttachmentTooLarge, "file is %d bytes; the limit is %d", len(ap.Data), MaxAttachmentBytes)
		}
		return nil
	}
	if ap.Data == nil {
		// A NULL data column means the bytes are gone; an empty file is
		// stored as an empty blob.
		ap.Data = []byte{}
	}
	sum := sha256.Sum256(ap.Data)
	digest := hex.EncodeToString(sum[:])
	size := int64(len(ap.Data))
	var out AttachResult
	err := s.act(ctx, p, ns, name, ap.Agent, check, func(tx *wtx, c *conv) error {
		if err := openOrError(c); err != nil {
			return err
		}
		if _, err := s.member(ctx, tx, c, ap.Agent); err != nil {
			return err
		}
		now := ms(s.now())
		var blobID int64
		var stored bool
		err := tx.QueryRowContext(ctx, `SELECT id, data IS NOT NULL FROM attachment_blobs
			WHERE conversation_id = ? AND sha256 = ?`, c.id, digest).Scan(&blobID, &stored)
		switch {
		case isNoRows(err):
			if err := s.makeRoom(ctx, tx, c, size); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO attachment_blobs (conversation_id, sha256, size, created_at, uploaded_at, data)
				VALUES (?, ?, ?, ?, ?, ?)`, c.id, digest, size, now, now, ap.Data)
			if err != nil {
				return err
			}
			if blobID, err = res.LastInsertId(); err != nil {
				return err
			}
		case err != nil:
			return err
		case stored:
			if _, err := tx.ExecContext(ctx, `UPDATE attachment_blobs SET uploaded_at = ? WHERE id = ?`, now, blobID); err != nil {
				return err
			}
		default:
			// The bytes were stored here before and are gone; store them again.
			if err := s.makeRoom(ctx, tx, c, size); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attachment_blobs SET uploaded_at = ?, gone = NULL, data = ? WHERE id = ?`,
				now, ap.Data, blobID); err != nil {
				return err
			}
		}
		m, err := s.appendMsg(ctx, tx, c, KindAttachment, ap.Agent, ap.Name, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments (conversation_id, seq, blob_id, name) VALUES (?, ?, ?, ?)`,
			c.id, m.Seq, blobID, ap.Name); err != nil {
			return err
		}
		if err := s.touch(ctx, tx, c, ap.Agent, PresenceWorking); err != nil {
			return err
		}
		m.Attachment = &Attachment{Name: ap.Name, Size: size, SHA256: digest}
		out.Message = m
		out.Standing, err = s.standing(ctx, tx, c, ap.Agent)
		return err
	})
	out.Message.YourAccount = p.owns(out.Message.account)
	return out, err
}

// makeRoom checks that size more bytes fit c's quota and the server cap,
// evicting bytes from concluded conversations, least recently uploaded
// first, until they do. The caller's transaction undoes the evictions if it
// fails.
func (s *Service) makeRoom(ctx context.Context, tx *wtx, c *conv, size int64) error {
	var inConv int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(size), 0) FROM attachment_blobs
		WHERE conversation_id = ? AND data IS NOT NULL`, c.id).Scan(&inConv); err != nil {
		return err
	}
	if inConv+size > s.opts.AttachmentQuota {
		return errorf(CodeAttachmentQuota, "conversation %q holds %d bytes of attachments; %d more would pass its quota of %d",
			c.Name, inConv, size, s.opts.AttachmentQuota)
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(size), 0) FROM attachment_blobs WHERE data IS NOT NULL`).Scan(&total); err != nil {
		return err
	}
	if total+size <= s.opts.AttachmentCap {
		return nil
	}
	victims, err := collect(ctx, tx, func(r *sql.Rows) ([2]int64, error) {
		var v [2]int64
		err := r.Scan(&v[0], &v[1])
		return v, err
	}, `SELECT b.id, b.size FROM attachment_blobs b JOIN conversations c ON c.id = b.conversation_id
		WHERE b.data IS NOT NULL AND `+concludedAt+` IS NOT NULL ORDER BY b.uploaded_at, b.id`, ms(s.now()))
	if err != nil {
		return err
	}
	for _, v := range victims {
		if total+size <= s.opts.AttachmentCap {
			break
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attachment_blobs SET data = NULL, gone = ? WHERE id = ?`, GoneEvicted, v[0]); err != nil {
			return err
		}
		total -= v[1]
	}
	if total+size > s.opts.AttachmentCap {
		return errorf(CodeAttachmentCap, "the server's attachment storage is full: open conversations hold %d of its %d bytes", total, s.opts.AttachmentCap)
	}
	return nil
}

// FetchParams name an attachment to download and who downloads it: Agent,
// a participant, or else an observer. An observer with an account is
// recorded under the account's name; the anonymous principal (auth mode
// none) under Observer.
type FetchParams struct {
	Seq      int64
	Agent    string
	Observer string
}

// Download is an attachment's description and bytes.
type Download struct {
	Attachment Attachment
	Data       []byte
}

// Fetch returns the file attached at seq. The first download by each
// participant or observer appends a system line saying so while the
// conversation is open; after it concludes the download is recorded but
// not announced. Bytes stay downloadable after the conversation concludes
// until they expire.
func (s *Service) Fetch(ctx context.Context, p Principal, ns, name string, fp FetchParams) (Download, error) {
	if err := s.requireAttachments(); err != nil {
		return Download{}, err
	}
	var out Download
	fn := func(who string) func(tx *wtx, c *conv) error {
		return func(tx *wtx, c *conv) error {
			var blobID int64
			var gone sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT b.id, a.name, b.size, b.sha256, b.gone, b.data FROM attachments a
				JOIN attachment_blobs b ON b.id = a.blob_id WHERE a.conversation_id = ? AND a.seq = ?`, c.id, fp.Seq).
				Scan(&blobID, &out.Attachment.Name, &out.Attachment.Size, &out.Attachment.SHA256, &gone, &out.Data)
			if isNoRows(err) {
				return errorf(CodeAttachmentNotFound, "#%d in conversation %q is not an attachment", fp.Seq, c.Name)
			}
			if err != nil {
				return err
			}
			if gone.Valid {
				out.Attachment.Gone = gone.String
				return errorf(CodeAttachmentGone, "the bytes of %+q (#%d) are no longer stored: %s", out.Attachment.Name, fp.Seq, gone.String)
			}
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attachment_downloads (blob_id, downloader, created_at)
				VALUES (?, ?, ?)`, blobID, who, ms(s.now()))
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 || c.Status != StatusOpen {
				return err
			}
			_, err = s.appendMsg(ctx, tx, c, KindSystem, who, who+" downloaded "+out.Attachment.Name+" (#"+strconv.FormatInt(fp.Seq, 10)+")", nil)
			return err
		}
	}
	if fp.Agent != "" {
		err := s.act(ctx, p, ns, name, fp.Agent, nil, func(tx *wtx, c *conv) error {
			pt, err := s.participant(ctx, tx, c.id, fp.Agent)
			if err != nil {
				return err
			}
			if pt == nil {
				return errorf(CodeNotParticipant, "%s never joined conversation %q", fp.Agent, c.Name)
			}
			return fn(fp.Agent)(tx, c)
		})
		return out, err
	}
	if err := s.authorize(ctx, p, ns, ActionRead); err != nil {
		return Download{}, err
	}
	if err := s.limitWrite(p); err != nil {
		return Download{}, err
	}
	who := fp.Observer
	if !p.IsAnonymous() {
		a, err := s.accountByID(ctx, s.db, p.AccountID)
		if err != nil {
			return Download{}, err
		}
		who = a.Name
	}
	if err := checkName("observer", who); err != nil {
		return Download{}, err
	}
	if err := s.settle(ctx, ns, name); err != nil {
		return Download{}, err
	}
	err := s.writeAs(ctx, p, ns, ActionRead, func(tx *wtx) error {
		c, err := s.loadConvTx(ctx, tx, ns, name)
		if err != nil {
			return err
		}
		return fn(who)(tx, c)
	})
	return out, err
}

// ExpireAttachments drops the bytes of attachments in conversations that
// concluded longer ago than Options.AttachmentExpiry, and returns how many
// blobs it dropped. The server runs it periodically.
func (s *Service) ExpireAttachments(ctx context.Context) (int64, error) {
	if !s.opts.Attachments {
		return 0, nil
	}
	cutoff := ms(s.now().Add(-s.opts.AttachmentExpiry))
	var n int64
	err := s.write(ctx, func(tx *wtx) error {
		res, err := tx.ExecContext(ctx, `UPDATE attachment_blobs SET data = NULL, gone = ?3
			WHERE data IS NOT NULL AND conversation_id IN
				(SELECT c.id FROM conversations c WHERE `+concludedAt+` <= ?2)`, ms(s.now()), cutoff, GoneExpired)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// AttachmentUsage is the attachment storage of one conversation.
type AttachmentUsage struct {
	Namespace    string `json:"namespace"`
	Conversation string `json:"conversation"`
	Status       Status `json:"status"`
	// Blobs and Bytes count stored files; Gone counts files whose bytes
	// are no longer stored.
	Blobs int   `json:"blobs"`
	Bytes int64 `json:"bytes"`
	Gone  int   `json:"gone"`
}

// AttachmentReport is the server's attachment storage by conversation,
// largest first, with its limits.
type AttachmentReport struct {
	Conversations []AttachmentUsage `json:"conversations"`
	Bytes         int64             `json:"bytes"`
	Cap           int64             `json:"cap"`
	Quota         int64             `json:"quota"`
}

// AttachmentStorage reports attachment storage by conversation. Server
// admins only.
func (s *Service) AttachmentStorage(ctx context.Context, p Principal) (AttachmentReport, error) {
	if err := s.requireServerAdmin(ctx, p); err != nil {
		return AttachmentReport{}, err
	}
	if err := s.requireAttachments(); err != nil {
		return AttachmentReport{}, err
	}
	out := AttachmentReport{Cap: s.opts.AttachmentCap, Quota: s.opts.AttachmentQuota}
	var err error
	out.Conversations, err = collect(ctx, s.db, func(r *sql.Rows) (AttachmentUsage, error) {
		var u AttachmentUsage
		err := r.Scan(&u.Namespace, &u.Conversation, &u.Status, &u.Blobs, &u.Bytes, &u.Gone)
		return u, err
	}, `SELECT n.name, c.name, c.status,
			count(*) FILTER (WHERE b.data IS NOT NULL),
			coalesce(sum(b.size) FILTER (WHERE b.data IS NOT NULL), 0),
			count(*) FILTER (WHERE b.data IS NULL)
		FROM attachment_blobs b
		JOIN conversations c ON c.id = b.conversation_id
		JOIN namespaces n ON n.id = c.namespace_id
		GROUP BY b.conversation_id ORDER BY 5 DESC, n.name, c.name`)
	for _, u := range out.Conversations {
		out.Bytes += u.Bytes
	}
	return out, err
}

// AttachmentDelete selects stored attachment bytes to delete: those with
// SHA256 in any conversation, or every one in Namespace/Conversation.
type AttachmentDelete struct {
	SHA256       string
	Namespace    string
	Conversation string
}

// DeleteAttachments drops the selected bytes, keeping the attachment
// messages, which then show them as deleted. It returns how many blobs and
// bytes it dropped. Server admins only.
func (s *Service) DeleteAttachments(ctx context.Context, p Principal, d AttachmentDelete) (blobs int64, bytes int64, err error) {
	if err := s.requireServerAdmin(ctx, p); err != nil {
		return 0, 0, err
	}
	if err := s.requireAttachments(); err != nil {
		return 0, 0, err
	}
	var where string
	var args []any
	switch {
	case d.SHA256 != "" && d.Namespace == "" && d.Conversation == "":
		where, args = `sha256 = ?`, []any{strings.ToLower(d.SHA256)}
	case d.SHA256 == "" && d.Namespace != "" && d.Conversation != "":
		where = `conversation_id = (SELECT c.id FROM conversations c JOIN namespaces n ON n.id = c.namespace_id
			WHERE n.name = ? AND c.name = ?)`
		args = []any{d.Namespace, d.Conversation}
		if _, err := s.loadConv(ctx, s.db, d.Namespace, d.Conversation); err != nil {
			return 0, 0, err
		}
	default:
		return 0, 0, errorf(CodeInvalid, "name a sha256, or a namespace and conversation")
	}
	err = s.writeAs(ctx, p, "", ActionAdmin, func(tx *wtx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size), 0) FROM attachment_blobs
			WHERE data IS NOT NULL AND `+where, args...).Scan(&blobs, &bytes); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE attachment_blobs SET data = NULL, gone = ? WHERE data IS NOT NULL AND `+where,
			append([]any{GoneDeleted}, args...)...)
		return err
	})
	return blobs, bytes, err
}

// withAttachments fills in Attachment on the attachment messages in ms, all
// from conversation convID.
func withAttachments(ctx context.Context, q querier, convID int64, ms []Message) error {
	var lo, hi int64
	for _, m := range ms {
		if m.Kind != KindAttachment {
			continue
		}
		if lo == 0 {
			lo = m.Seq
		}
		hi = m.Seq
	}
	if lo == 0 {
		return nil
	}
	type row struct {
		seq int64
		a   Attachment
	}
	rows, err := collect(ctx, q, func(r *sql.Rows) (row, error) {
		var v row
		err := r.Scan(&v.seq, &v.a.Name, &v.a.Size, &v.a.SHA256, &v.a.Gone)
		return v, err
	}, `SELECT a.seq, a.name, b.size, b.sha256, coalesce(b.gone, '') FROM attachments a
		JOIN attachment_blobs b ON b.id = a.blob_id
		WHERE a.conversation_id = ? AND a.seq BETWEEN ? AND ?`, convID, lo, hi)
	if err != nil {
		return err
	}
	bySeq := make(map[int64]Attachment, len(rows))
	for _, r := range rows {
		bySeq[r.seq] = r.a
	}
	for i := range ms {
		if a, ok := bySeq[ms[i].Seq]; ok && ms[i].Kind == KindAttachment {
			ms[i].Attachment = &a
		}
	}
	return nil
}
