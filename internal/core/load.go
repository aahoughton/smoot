package core

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aahoughton/smoot/internal/store"
)

// LoadResult is what LoadNamespace added.
type LoadResult struct {
	Namespace     string `json:"namespace"`
	Conversations int    `json:"conversations"`
	Messages      int    `json:"messages"`
}

// LoadNamespace adds the namespace in a namespace dump to this database,
// named as (the dump's own name when empty), which must not exist yet.
// Server admins only. The dump is first loaded into a scratch database
// beside this one, which applies every check LoadDump makes and migrates it
// to this build's schema; its rows are then copied in one transaction with
// new IDs. Like any namespace dump, it grants nothing: give accounts roles
// in the namespace afterwards.
func (s *Service) LoadNamespace(ctx context.Context, p Principal, r io.Reader, as string) (LoadResult, error) {
	if err := s.requireServerAdmin(ctx, p); err != nil {
		return LoadResult{}, err
	}
	br := bufio.NewReaderSize(r, 1<<20)
	h, err := ReadDumpHeader(br, store.LatestVersion())
	if err != nil {
		return LoadResult{}, errorf(CodeInvalid, "%v", err)
	}
	if h.Namespace == "" {
		return LoadResult{}, errorf(CodeInvalid, "load: this is a full dump; a running server loads only namespace dumps")
	}
	if as == "" {
		as = h.Namespace
	}
	if err := checkName("namespace", as); err != nil {
		return LoadResult{}, err
	}
	// Refuse early, before staging; the write below checks again.
	if _, err := namespaceID(ctx, s.db, as); err == nil {
		return LoadResult{}, errorf(CodeNamespaceExists, "namespace %q already exists; load it under another name", as)
	} else if CodeOf(err) != CodeNamespaceNotFound {
		return LoadResult{}, err
	}

	src, cleanup, err := s.stage(ctx, h, br)
	if err != nil {
		return LoadResult{}, err
	}
	defer cleanup()

	res := LoadResult{Namespace: as}
	err = s.writeAs(ctx, p, "", ActionAdmin, func(tx *wtx) error {
		if _, err := namespaceID(ctx, tx, as); err == nil {
			return errorf(CodeNamespaceExists, "namespace %q already exists; load it under another name", as)
		} else if CodeOf(err) != CodeNamespaceNotFound {
			return err
		}
		var srcNS, nsID int64
		if err := src.QueryRowContext(ctx, `SELECT id FROM namespaces WHERE name = ?`, h.Namespace).Scan(&srcNS); err != nil {
			return fmt.Errorf("load: the dump has no namespace %q: %w", h.Namespace, err)
		}
		if _, err := copyRows(ctx, src, tx, "namespaces", `SELECT * FROM namespaces WHERE id = ?`, []any{srcNS},
			func(row map[string]any) { row["name"] = as },
			func(_ map[string]any, id int64) { nsID = id }); err != nil {
			return err
		}
		convs := map[int64]int64{}
		n, err := copyRows(ctx, src, tx, "conversations", `SELECT * FROM conversations WHERE namespace_id = ?`, []any{srcNS},
			func(row map[string]any) { row["namespace_id"] = nsID },
			func(row map[string]any, id int64) { convs[row["id"].(int64)] = id })
		if err != nil {
			return err
		}
		res.Conversations = n
		remap := func(row map[string]any) { row["conversation_id"] = convs[row["conversation_id"].(int64)] }
		inNS := `WHERE conversation_id IN (SELECT id FROM conversations WHERE namespace_id = ?)`
		if _, err := copyRows(ctx, src, tx, "participants", `SELECT * FROM participants `+inNS, []any{srcNS}, remap, nil); err != nil {
			return err
		}
		res.Messages, err = copyRows(ctx, src, tx, "messages", `SELECT * FROM messages `+inNS+` ORDER BY conversation_id, seq`, []any{srcNS}, remap, nil)
		if err != nil {
			return err
		}
		blobs := map[int64]int64{}
		if _, err := copyRows(ctx, src, tx, "attachment_blobs", `SELECT * FROM attachment_blobs `+inNS, []any{srcNS}, remap,
			func(row map[string]any, id int64) { blobs[row["id"].(int64)] = id }); err != nil {
			return err
		}
		remapBlob := func(row map[string]any) { row["blob_id"] = blobs[row["blob_id"].(int64)] }
		if _, err := copyRows(ctx, src, tx, "attachments", `SELECT * FROM attachments `+inNS, []any{srcNS},
			func(row map[string]any) { remap(row); remapBlob(row) }, nil); err != nil {
			return err
		}
		_, err = copyRows(ctx, src, tx, "attachment_downloads", `SELECT * FROM attachment_downloads
			WHERE blob_id IN (SELECT id FROM attachment_blobs `+inNS+`)`, []any{srcNS}, remapBlob, nil)
		return err
	})
	if err != nil {
		return LoadResult{}, err
	}
	return res, nil
}

// RequireServerAdmin refuses a caller who is not a server admin. Surfaces
// call it before work that core would refuse anyway, such as receiving a
// large upload.
func (s *Service) RequireServerAdmin(ctx context.Context, p Principal) error {
	return s.requireServerAdmin(ctx, p)
}

// stage loads a dump into a scratch database in a private directory beside
// this one and migrates it. cleanup closes and removes it.
func (s *Service) stage(ctx context.Context, h DumpHeader, r *bufio.Reader) (*sql.DB, func(), error) {
	var file string
	if err := s.db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&file); err != nil {
		return nil, nil, err
	}
	if file == "" {
		return nil, nil, errors.New("load: the database has no file")
	}
	dir, err := os.MkdirTemp(filepath.Dir(file), ".smoot-load-")
	if err != nil {
		return nil, nil, err
	}
	removeDir := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "smoot.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		removeDir()
		return nil, nil, err
	}
	if err := f.Close(); err != nil {
		removeDir()
		return nil, nil, err
	}
	db, err := store.OpenAt(ctx, path, h.Schema)
	if err != nil {
		removeDir()
		return nil, nil, err
	}
	_, err = LoadDump(ctx, db, h, r, s.now())
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		removeDir()
		return nil, nil, errorf(CodeInvalid, "%v", err)
	}
	if db, err = store.Open(ctx, path); err != nil {
		removeDir()
		return nil, nil, err
	}
	return db, func() { _ = db.Close(); removeDir() }, nil
}

// copyRows inserts the rows query selects from src into table, all columns
// but id. adjust may change a row before it is inserted; after, if set,
// gets the source row and the new row's id.
func copyRows(ctx context.Context, src *sql.DB, tx *wtx, table, query string, args []any,
	adjust func(row map[string]any), after func(row map[string]any, id int64),
) (int, error) {
	rows, err := src.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	var names []string
	for _, c := range cols {
		if c != "id" {
			names = append(names, `"`+c+`"`)
		}
	}
	insert := "INSERT INTO " + table + " (" + strings.Join(names, ", ") + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		if adjust != nil {
			adjust(row)
		}
		args := make([]any, 0, len(names))
		for _, c := range cols {
			if c != "id" {
				args = append(args, row[c])
			}
		}
		res, err := tx.ExecContext(ctx, insert, args...)
		if err != nil {
			return n, fmt.Errorf("load %s: %w", table, err)
		}
		if after != nil {
			id, err := res.LastInsertId()
			if err != nil {
				return n, err
			}
			after(row, id)
		}
		n++
	}
	return n, rows.Err()
}
