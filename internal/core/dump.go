package core

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A dump is JSON Lines: a header, then one record per table row in an order
// that satisfies foreign keys. Rows are copied column by column, so a dump
// is tied to the schema version in its header; Load creates the database at
// that version and migrates it afterwards.
//
// A namespace dump is portable and grants nothing: it holds one namespace's
// conversations, participants, messages and attachment records, with every
// account reference cleared, and no accounts, tokens, memberships or
// feedback. A full dump holds every table except browser sessions, with
// IDs, roles and token hashes. Neither holds attachment bytes: each blob
// is dumped as gone, "missing" unless it was already gone for another
// reason.

// DumpFormat and DumpVersion identify the dump format.
const (
	DumpFormat  = "smoot-dump"
	DumpVersion = 1
)

// DumpHeader is a dump's first line.
type DumpHeader struct {
	Format string `json:"format"`
	// Version is the dump format version.
	Version int `json:"version"`
	// Schema is the database schema version the rows come from.
	Schema int `json:"schema"`
	// Namespace is set for a namespace dump; empty for a full dump.
	Namespace string `json:"namespace,omitempty"`
	CreatedAt string `json:"created_at"`
}

// dumpRow is one table row, or the end record, which counts the rows
// before it so a cut-off dump is detected.
type dumpRow struct {
	Table string         `json:"table,omitempty"`
	Row   map[string]any `json:"row,omitempty"`
	End   bool           `json:"end,omitempty"`
	Rows  int            `json:"rows,omitempty"`
}

// blob marks a BLOB column value in a dump, base64 encoded.
type blob struct {
	B64 string `json:"blob"`
}

// dumpTables lists the tables a dump may hold, in load order.
var dumpTables = []string{
	"namespaces", "accounts", "tokens", "memberships", "conversations", "participants", "messages", "feedback",
	"attachment_blobs", "attachments", "attachment_downloads",
}

// dumpSelects replace SELECT * for tables a dump does not copy whole.
var dumpSelects = map[string]string{
	"attachment_blobs": `SELECT id, conversation_id, sha256, size, created_at, uploaded_at,
		coalesce(gone, '` + GoneMissing + `') AS gone FROM attachment_blobs`,
}

// accountColumnsByTable are the account references a namespace dump clears.
var accountColumnsByTable = map[string][]string{
	"conversations": {"created_by_account"},
	"participants":  {"account_id"},
	"messages":      {"account_id"},
}

// Dump writes a dump of namespace ns (or, with ns empty, of everything) to
// w. A namespace dump needs that namespace's admin role; a full dump needs a
// server admin. The dump reads one consistent snapshot without holding up
// writers.
func (s *Service) Dump(ctx context.Context, p Principal, ns string, w io.Writer) error {
	if err := s.authorize(ctx, p, ns, ActionAdmin); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// A deferred read transaction on one connection: a WAL snapshot that
	// writers do not wait for.
	if _, err := conn.ExecContext(ctx, "BEGIN DEFERRED"); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK") }()

	var schema int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(DumpHeader{
		Format: DumpFormat, Version: DumpVersion, Schema: schema, Namespace: ns,
		CreatedAt: s.now().Format("2006-01-02T15:04:05.000Z07:00"),
	}); err != nil {
		return err
	}

	tables := dumpTables
	var nsID int64
	if ns != "" {
		if nsID, err = namespaceID(ctx, conn, ns); err != nil {
			return err
		}
		tables = []string{"namespaces", "conversations", "participants", "messages", "attachment_blobs", "attachments", "attachment_downloads"}
	}
	total := 0
	for _, table := range tables {
		query := "SELECT * FROM " + table
		if q, ok := dumpSelects[table]; ok {
			query = q
		}
		var args []any
		if ns != "" {
			switch table {
			case "namespaces":
				query += " WHERE id = ?"
			case "conversations":
				query += " WHERE namespace_id = ?"
			case "attachment_downloads":
				query += ` WHERE blob_id IN (SELECT id FROM attachment_blobs
					WHERE conversation_id IN (SELECT id FROM conversations WHERE namespace_id = ?))`
			default:
				query += " WHERE conversation_id IN (SELECT id FROM conversations WHERE namespace_id = ?)"
			}
			args = append(args, nsID)
		}
		n, err := dumpTable(ctx, conn, enc, table, query, args, ns != "")
		if err != nil {
			return fmt.Errorf("dump %s: %w", table, err)
		}
		total += n
	}
	if err := enc.Encode(dumpRow{End: true, Rows: total}); err != nil {
		return err
	}
	return bw.Flush()
}

func dumpTable(ctx context.Context, q querier, enc *json.Encoder, table, query string, args []any, portable bool) (n int, err error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte:
				row[c] = blob{B64: base64.StdEncoding.EncodeToString(v)}
			default:
				row[c] = v
			}
		}
		if portable {
			for _, c := range accountColumnsByTable[table] {
				if _, ok := row[c]; ok {
					row[c] = nil
				}
			}
		}
		if err := enc.Encode(dumpRow{Table: table, Row: row}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// LoadDump reads a dump from r into db, which must be newly created at the
// dump's schema version (see ReadDumpHeader) and hold nothing but the
// default namespace. It runs in one transaction: any bad row, unknown table
// or column, or dangling reference rolls the whole load back. now bounds
// the times the dump may hold.
func LoadDump(ctx context.Context, db *sql.DB, h DumpHeader, r *bufio.Reader, now time.Time) (rows int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM conversations) + (SELECT count(*) FROM accounts)
		+ (SELECT count(*) FROM namespaces WHERE name != 'default')`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, errors.New("load: the target database is not empty")
	}
	// The dump brings its own namespaces; default is added back if it
	// does not.
	if _, err := tx.ExecContext(ctx, `DELETE FROM namespaces`); err != nil {
		return 0, err
	}
	columns := map[string]map[string]bool{}
	for _, t := range dumpTables {
		cols, err := tableColumns(ctx, tx, t)
		if err != nil {
			return 0, err
		}
		columns[t] = cols
	}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	ended := false
	for line := 2; ; line++ {
		var dr dumpRow
		if err := dec.Decode(&dr); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, fmt.Errorf("load: record %d: %w", line, err)
		}
		if ended {
			return 0, fmt.Errorf("load: record %d follows the end of the dump", line)
		}
		if dr.End {
			if dr.Rows != rows {
				return 0, fmt.Errorf("load: the dump says it has %d rows, but %d came before its end", dr.Rows, rows)
			}
			ended = true
			continue
		}
		// A table the dump's schema predates has no columns.
		cols, ok := columns[dr.Table]
		if !ok || len(cols) == 0 || (h.Namespace != "" && (dr.Table == "accounts" || dr.Table == "tokens" || dr.Table == "memberships" || dr.Table == "feedback")) {
			return 0, fmt.Errorf("load: record %d: table %q does not belong in this dump", line, dr.Table)
		}
		names := make([]string, 0, len(dr.Row))
		args := make([]any, 0, len(dr.Row))
		for c, v := range dr.Row {
			if !cols[c] {
				return 0, fmt.Errorf("load: record %d: %s has no column %q", line, dr.Table, c)
			}
			val, err := loadValue(v)
			if err != nil {
				return 0, fmt.Errorf("load: record %d: %s.%s: %w", line, dr.Table, c, err)
			}
			names = append(names, `"`+c+`"`)
			args = append(args, val)
		}
		q := "INSERT INTO " + dr.Table + " (" + strings.Join(names, ", ") + ") VALUES (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return 0, fmt.Errorf("load: record %d (%s): %w", line, dr.Table, err)
		}
		rows++
	}
	if !ended {
		return 0, errors.New("load: the dump is cut off (no end record)")
	}
	// A dump from an older schema is loaded into a database without the
	// later tables and columns, such as feedback (6) and seen_seq (7).
	if err := checkLoaded(ctx, tx, now, columns); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO namespaces (name, created_at)
		VALUES ('default', CAST(unixepoch('subsec') * 1000 AS INTEGER))`); err != nil {
		return 0, err
	}
	fk, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return 0, err
	}
	dangling := fk.Next()
	_ = fk.Close()
	if dangling {
		return 0, errors.New("load: the dump refers to rows it does not contain")
	}
	return rows, tx.Commit()
}

// ReadDumpHeader reads and checks a dump's header line.
func ReadDumpHeader(r *bufio.Reader, latestSchema int) (DumpHeader, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return DumpHeader{}, err
	}
	var h DumpHeader
	if err := json.Unmarshal(line, &h); err != nil || h.Format != DumpFormat {
		return DumpHeader{}, errors.New("load: not a smoot dump")
	}
	if h.Version != DumpVersion {
		return DumpHeader{}, fmt.Errorf("load: dump format version %d; this build reads version %d", h.Version, DumpVersion)
	}
	if h.Schema < 1 || h.Schema > latestSchema {
		return DumpHeader{}, fmt.Errorf("load: dump schema version %d; this build knows 1-%d", h.Schema, latestSchema)
	}
	return h, nil
}

func tableColumns(ctx context.Context, q querier, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[c] = true
	}
	return cols, rows.Err()
}

func loadValue(v any) (any, error) {
	switch v := v.(type) {
	case nil, string:
		return v, nil
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		return v.Float64()
	case map[string]any:
		s, ok := v["blob"].(string)
		if !ok || len(v) != 1 {
			return nil, errors.New("unexpected object")
		}
		return base64.StdEncoding.DecodeString(s)
	default:
		return nil, fmt.Errorf("unexpected %T", v)
	}
}

// Backup makes a consistent copy of the whole database (an SQLite file) and
// passes its size and contents to send. Server admins only. The copy is
// made by VACUUM INTO in a private temporary directory next to the
// database, and removed afterwards.
func (s *Service) Backup(ctx context.Context, p Principal, send func(size int64, r io.Reader) error) error {
	if err := s.authorize(ctx, p, "", ActionAdmin); err != nil {
		return err
	}
	var seq int
	var name, file string
	if err := s.db.QueryRowContext(ctx, "SELECT seq, name, file FROM pragma_database_list WHERE name = 'main'").
		Scan(&seq, &name, &file); err != nil {
		return err
	}
	if file == "" {
		return errors.New("backup: the database has no file")
	}
	dir, err := os.MkdirTemp(filepath.Dir(file), ".smoot-backup-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	copyPath := filepath.Join(dir, "smoot.db")
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", copyPath); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	f, err := os.Open(copyPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return send(info.Size(), f)
}

// loadClockSkew is how far in the future a loaded time may be.
const loadClockSkew = 24 * time.Hour

// checkLoaded verifies the rules the rest of core relies on, which foreign
// keys alone do not: each conversation's log runs 1..last_seq without gaps,
// read positions lie within the log, replies point back, enumerations hold
// known values, names (including message authors and creators) are valid,
// text stays within the limits live writes enforce, a timebox has an end
// time, and no time lies in the future. columns holds the loaded tables'
// columns; checks of a table or column it lacks are skipped.
func checkLoaded(ctx context.Context, tx *sql.Tx, now time.Time, columns map[string]map[string]bool) error {
	// Times may not lie in the future beyond clock skew between servers.
	latest := now.Add(loadClockSkew).UnixMilli()
	checks := []struct {
		what, query string
		args        []any
	}{
		{"messages do not run 1..last_seq in conversation", `SELECT c.name FROM conversations c
			WHERE c.last_seq != (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id)
			OR c.last_seq != coalesce((SELECT max(seq) FROM messages m WHERE m.conversation_id = c.id), 0)
			OR EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.seq < 1)`, nil},
		{"a read position is outside the log of conversation", `SELECT c.name FROM participants p
			JOIN conversations c ON c.id = p.conversation_id WHERE p.read_seq < 0 OR p.read_seq > c.last_seq`, nil},
		{"a reply points forward in conversation", `SELECT c.name FROM messages m
			JOIN conversations c ON c.id = m.conversation_id WHERE m.reply_to IS NOT NULL AND (m.reply_to < 1 OR m.reply_to >= m.seq)`, nil},
		{"unknown participant state or presence in conversation", `SELECT c.name FROM participants p
			JOIN conversations c ON c.id = p.conversation_id
			WHERE p.state NOT IN ('active', 'concluded', 'left', 'dropped') OR p.presence NOT IN ('working', 'waiting', 'away')`, nil},
		{"unknown message kind in conversation", `SELECT c.name FROM messages m
			JOIN conversations c ON c.id = m.conversation_id WHERE m.kind NOT IN ('say', 'conclude', 'observer', 'system', 'attachment')`, nil},
		{"unknown status or end condition for conversation", `SELECT name FROM conversations
			WHERE status NOT IN ('open', 'concluded') OR end_condition NOT IN ('none', 'agreement', 'coordinator', 'timebox')`, nil},
		{"unknown role for account", `SELECT a.name FROM memberships m JOIN accounts a ON a.id = m.account_id
			WHERE m.role NOT IN ('viewer', 'observer', 'member', 'admin')`, nil},
		{"an end time that does not match the end condition of conversation", `SELECT name FROM conversations
			WHERE (end_condition = 'timebox') != (ends_at IS NOT NULL)`, nil},
		{"a topic that is not one line of at most 500 bytes in conversation", `SELECT name FROM conversations
			WHERE instr(topic, char(10)) > 0 OR instr(topic, char(13)) > 0 OR length(CAST(topic AS BLOB)) > ?`, []any{maxTopicBytes}},
		{"a note longer than 500 bytes in conversation", `SELECT c.name FROM participants p
			JOIN conversations c ON c.id = p.conversation_id WHERE length(CAST(p.note AS BLOB)) > ?`, []any{maxNoteBytes}},
		{"a message over the body limit in conversation", `SELECT c.name FROM messages m
			JOIN conversations c ON c.id = m.conversation_id WHERE length(CAST(m.body AS BLOB)) > ?`, []any{DefaultMaxBodyBytes}},
		{
			"a time in the future in conversation", `SELECT name FROM conversations
			WHERE created_at > ?1 OR last_at > ?1 OR coalesce(concluded_at, 0) > ?1
			OR EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = conversations.id AND m.created_at > ?1)
			OR EXISTS (SELECT 1 FROM participants p WHERE p.conversation_id = conversations.id AND (p.joined_at > ?1 OR p.last_seen_at > ?1))`,
			[]any{latest},
		},
	}
	if columns["messages"]["seen_seq"] {
		checks = append(checks, struct {
			what, query string
			args        []any
		}{"an invalid message read position in conversation", `SELECT c.name FROM messages m
			JOIN conversations c ON c.id = m.conversation_id WHERE m.seen_seq IS NOT NULL
			AND (typeof(m.seen_seq) != 'integer' OR m.kind NOT IN ('say', 'conclude') OR m.seen_seq < 0 OR m.seen_seq >= m.seq)`, nil})
	}
	if len(columns["feedback"]) > 0 {
		checks = append(checks, []struct {
			what, query string
			args        []any
		}{
			{"a feedback note that is empty or over the limit in conversation", `SELECT c.name FROM feedback f
			JOIN conversations c ON c.id = f.conversation_id
			WHERE trim(f.body, ' ' || char(9, 10, 13)) = '' OR length(CAST(f.body AS BLOB)) > ?`, []any{MaxFeedbackBytes}},
			{"a feedback read position outside the log of conversation", `SELECT c.name FROM feedback f
			JOIN conversations c ON c.id = f.conversation_id WHERE f.seen_seq < 0 OR f.seen_seq > c.last_seq`, nil},
			{"a feedback time in the future or out of order in conversation", `SELECT c.name FROM feedback f
			JOIN conversations c ON c.id = f.conversation_id WHERE f.updated_at > ? OR f.created_at > f.updated_at`, []any{latest}},
		}...)
	}
	if len(columns["attachments"]) > 0 {
		checks = append(checks, []struct {
			what, query string
			args        []any
		}{
			{"attachment bytes in the dump, or a blob without a known gone reason, in conversation", `SELECT c.name FROM attachment_blobs b
			JOIN conversations c ON c.id = b.conversation_id
			WHERE b.data IS NOT NULL OR coalesce(b.gone, '') NOT IN ('expired', 'evicted', 'deleted', 'missing')`, nil},
			{"an invalid attachment size or sha256 in conversation", `SELECT c.name FROM attachment_blobs b
			JOIN conversations c ON c.id = b.conversation_id
			WHERE typeof(b.size) != 'integer' OR b.size < 0 OR b.size > ?
			OR length(b.sha256) != 64 OR b.sha256 GLOB '*[^0-9a-f]*'`, []any{MaxAttachmentBytes}},
			{"an attachment message without its record, or a record on another kind of message, in conversation", `SELECT c.name FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			WHERE (m.kind = 'attachment') != EXISTS (SELECT 1 FROM attachments a WHERE a.conversation_id = m.conversation_id AND a.seq = m.seq)`, nil},
			{"an attachment record whose blob is in another conversation, or named unlike its message, in conversation", `SELECT c.name FROM attachments a
			JOIN conversations c ON c.id = a.conversation_id
			JOIN attachment_blobs b ON b.id = a.blob_id
			JOIN messages m ON m.conversation_id = a.conversation_id AND m.seq = a.seq
			WHERE b.conversation_id != a.conversation_id OR m.body != a.name`, nil},
			{"an attachment time in the future in conversation", `SELECT c.name FROM attachment_blobs b
			JOIN conversations c ON c.id = b.conversation_id
			WHERE b.created_at > ?1 OR b.uploaded_at > ?1 OR b.created_at > b.uploaded_at
			OR EXISTS (SELECT 1 FROM attachment_downloads d WHERE d.blob_id = b.id AND d.created_at > ?1)`, []any{latest}},
		}...)
	}
	for _, c := range checks {
		var name string
		err := tx.QueryRowContext(ctx, c.query+" LIMIT 1", c.args...).Scan(&name)
		if err == nil {
			return fmt.Errorf("load: %s %q", c.what, name)
		}
		if !isNoRows(err) {
			return err
		}
	}
	names := []struct{ kind, query string }{
		{"namespace", "SELECT name FROM namespaces"},
		{"conversation", "SELECT name FROM conversations"},
		{"agent", "SELECT agent FROM participants"},
		{"message author", "SELECT DISTINCT author FROM messages"},
		{"conversation creator", "SELECT created_by FROM conversations"},
		{"account", "SELECT name FROM accounts"},
	}
	if len(columns["attachments"]) > 0 {
		names = append(names, struct{ kind, query string }{"attachment downloader", "SELECT DISTINCT downloader FROM attachment_downloads"})
	}
	if len(columns["attachments"]) > 0 {
		files, err := collect(ctx, tx, func(r *sql.Rows) (string, error) {
			var v string
			return v, r.Scan(&v)
		}, "SELECT name FROM attachments")
		if err != nil {
			return err
		}
		for _, f := range files {
			if err := CheckFileName(f); err != nil {
				return fmt.Errorf("load: invalid attachment: %w", err)
			}
		}
	}
	for _, n := range names {
		rows, err := tx.QueryContext(ctx, n.query)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				_ = rows.Close()
				return err
			}
			if !ValidName(v) {
				_ = rows.Close()
				return fmt.Errorf("load: invalid %s name %q", n.kind, v)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}
