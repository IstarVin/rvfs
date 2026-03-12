CREATE TABLE IF NOT EXISTS files (
	path          TEXT PRIMARY KEY,
	is_dir        INTEGER NOT NULL DEFAULT 0,
	size          INTEGER NOT NULL DEFAULT 0,
	mode          INTEGER NOT NULL DEFAULT 0,
	remote_mtime  INTEGER NOT NULL DEFAULT 0,
	local_mtime   INTEGER NOT NULL DEFAULT 0,
	cache_path    TEXT    NOT NULL DEFAULT '',
	state         TEXT    NOT NULL DEFAULT 'clean',
	cached_ranges TEXT    NOT NULL DEFAULT '[]',
	sync_error    TEXT    NOT NULL DEFAULT '',
	retry_after   INTEGER NOT NULL DEFAULT 0,
	pinned        INTEGER NOT NULL DEFAULT 0,
	last_access   INTEGER NOT NULL DEFAULT 0,
	checksum      TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_files_state ON files(state);
CREATE INDEX IF NOT EXISTS idx_files_dir   ON files(path) WHERE is_dir = 1;

CREATE INDEX IF NOT EXISTS idx_files_pinned      ON files(pinned);
CREATE INDEX IF NOT EXISTS idx_files_last_access ON files(last_access);
