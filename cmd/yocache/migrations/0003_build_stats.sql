-- +goose Up
ALTER TABLE blobs ADD COLUMN buildname TEXT;
ALTER TABLE blobs ADD COLUMN build_ms INTEGER; -- milliseconds; whole seconds truncate fast tasks to 0

CREATE TABLE IF NOT EXISTS build_downloads (
	buildname    TEXT    NOT NULL,
	kind         TEXT    NOT NULL,
	artifact_key TEXT    NOT NULL, -- sstateChecksum(path) for sstate, path for downloads
	bytes        INTEGER NOT NULL,
	ms           INTEGER NOT NULL DEFAULT 0, -- milliseconds saved, looked up from blobs.build_ms
	fetch_count  INTEGER NOT NULL DEFAULT 1,
	first_seen   INTEGER NOT NULL, -- Unix epoch seconds
	last_seen    INTEGER NOT NULL, -- Unix epoch seconds; used as the --build-stats-ttl cutoff
	PRIMARY KEY (buildname, kind, artifact_key)
);
CREATE INDEX IF NOT EXISTS build_downloads_by_last_seen ON build_downloads (last_seen);

-- +goose Down
DROP INDEX IF EXISTS build_downloads_by_last_seen;
DROP TABLE IF EXISTS build_downloads;
ALTER TABLE blobs DROP COLUMN build_ms;
ALTER TABLE blobs DROP COLUMN buildname;
