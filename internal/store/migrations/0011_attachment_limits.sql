-- Attachment limits for one conversation, set by a server admin: the
-- largest file it takes and the bytes it may store. NULL means the server's
-- default.
ALTER TABLE conversations ADD COLUMN attachment_max_bytes INTEGER;
ALTER TABLE conversations ADD COLUMN attachment_quota_bytes INTEGER;

-- 1 when a person watching the conversation uploaded the file, 0 when a
-- participant did.
ALTER TABLE attachments ADD COLUMN observer INTEGER NOT NULL DEFAULT 0;

-- 1 for a message that does not call on the agents by itself: a file an
-- observer uploaded, or the line recording an observer's download. It
-- counts as new once another message follows it.
ALTER TABLE messages ADD COLUMN quiet INTEGER NOT NULL DEFAULT 0;
