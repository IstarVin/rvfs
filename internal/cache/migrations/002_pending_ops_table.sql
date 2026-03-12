CREATE TABLE IF NOT EXISTS pending_ops (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	op         TEXT    NOT NULL,
	path       TEXT    NOT NULL,
	dest_path  TEXT    NOT NULL DEFAULT '',
	queued_at  INTEGER NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	last_error TEXT    NOT NULL DEFAULT ''
);
