CREATE TABLE IF NOT EXISTS drive_path_ids (
	path      TEXT PRIMARY KEY,
	drive_id  TEXT    NOT NULL,
	etag      TEXT    NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL DEFAULT 0
);
