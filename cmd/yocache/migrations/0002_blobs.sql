-- +goose Up
CREATE TABLE IF NOT EXISTS blobs (
	kind        TEXT    NOT NULL,
	path        TEXT    NOT NULL,
	size        INTEGER NOT NULL,
	added_at    INTEGER NOT NULL DEFAULT (unixepoch()),  -- Unix epoch seconds; auto-filled if omitted
	accessed_at INTEGER NOT NULL,                        -- Unix epoch seconds; updated on every cache-hit GET
	PRIMARY KEY (kind, path)
);
CREATE INDEX IF NOT EXISTS blobs_by_accessed ON blobs (accessed_at);

-- +goose Down
DROP INDEX IF EXISTS blobs_by_accessed;
DROP TABLE IF EXISTS blobs;
