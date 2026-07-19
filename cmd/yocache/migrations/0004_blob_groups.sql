-- +goose Up
-- checksum groups a task's archive with its .siginfo/.sig sidecars (same
-- content hash, different suffix) so eviction and hash-equiv cleanup can
-- treat them as one unit. NULL for downloads, which have no siblings.
ALTER TABLE blobs ADD COLUMN checksum TEXT;
CREATE INDEX IF NOT EXISTS blobs_by_checksum ON blobs (kind, checksum);

-- Backs hashEquivStore.DeleteByUnihash's per-taskhash outhashes cleanup.
CREATE INDEX IF NOT EXISTS outhashes_by_taskhash ON outhashes (method, taskhash);

-- Backfill checksum for sstate rows that predate this column, mirroring
-- sstateChecksum() in stats.go: split path on ':' and take the last field
-- ("$[#-1]" is SQLite's JSON path for the last array index), then split
-- that field on '_' and take the first one, e.g.
--   sstate:ninja-native::1.13.2:r0::14:37001365ba34_patch.tar.zst
--   split(':') last  -> "37001365ba34_patch.tar.zst"
--   split('_') first -> "37001365ba34"                -> checksum
UPDATE blobs
SET checksum = json_extract('["' || replace(
		json_extract('["' || replace(path, ':', '","') || '"]', '$[#-1]'),
	'_', '","') || '"]', '$[0]')
WHERE kind = 'sstate' AND checksum IS NULL;

-- +goose Down
DROP INDEX IF EXISTS outhashes_by_taskhash;
DROP INDEX IF EXISTS blobs_by_checksum;
ALTER TABLE blobs DROP COLUMN checksum;
