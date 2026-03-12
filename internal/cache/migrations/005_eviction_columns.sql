ALTER TABLE files ADD COLUMN pinned      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN last_access INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_files_pinned      ON files(pinned);
CREATE INDEX IF NOT EXISTS idx_files_last_access ON files(last_access);
