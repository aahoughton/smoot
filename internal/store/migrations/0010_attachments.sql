-- Files participants hand each other. A blob is one file's bytes in one
-- conversation, stored once however many times it is uploaded there. data
-- is NULL once the bytes are gone, and gone says why:
-- expired | evicted | deleted | missing (not in the dump it was loaded from).
-- data is the last column so scans of the others skip its overflow pages.
CREATE TABLE attachment_blobs (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations (id),
    sha256          TEXT    NOT NULL,
    size            INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    uploaded_at     INTEGER NOT NULL,
    gone            TEXT,
    data            BLOB,
    UNIQUE (conversation_id, sha256)
);

-- Eviction takes stored blobs least recently uploaded first.
CREATE INDEX attachment_blobs_stored ON attachment_blobs (uploaded_at, id)
    WHERE data IS NOT NULL;

-- One row per upload: the attachment message at seq and the name it was
-- uploaded under.
CREATE TABLE attachments (
    conversation_id INTEGER NOT NULL,
    seq             INTEGER NOT NULL,
    blob_id         INTEGER NOT NULL REFERENCES attachment_blobs (id),
    name            TEXT    NOT NULL,
    PRIMARY KEY (conversation_id, seq),
    FOREIGN KEY (conversation_id, seq) REFERENCES messages (conversation_id, seq)
) WITHOUT ROWID;

-- Who has downloaded each blob, so each downloader is recorded once.
CREATE TABLE attachment_downloads (
    blob_id    INTEGER NOT NULL REFERENCES attachment_blobs (id),
    downloader TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (blob_id, downloader)
) WITHOUT ROWID;
