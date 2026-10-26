-- +goose Up
-- One row per report rather than per output, laid out like bitbake hashserv's
-- outhashes_v2: every taskhash that produced an output stays on record, so
-- cross-output equivalence can fall back to the next-oldest reporter when the
-- oldest one's sstate archive is gone. The unihash is no longer stored here —
-- it's a decision, and lives only in unihashes (readers join to it).
-- created is an ISO 8601 UTC timestamp as upstream writes it; NULL (copied
-- rows below) sorts first, which is right: each was its output's only and
-- earliest reporter.
CREATE TABLE IF NOT EXISTS outhashes_v2 (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	method          TEXT NOT NULL,
	taskhash        TEXT NOT NULL,
	outhash         TEXT NOT NULL,
	created         DATETIME,
	owner           TEXT,
	PN              TEXT,
	PV              TEXT,
	PR              TEXT,
	task            TEXT,
	outhash_siginfo TEXT,
	UNIQUE (method, taskhash, outhash)
);
CREATE INDEX IF NOT EXISTS outhash_lookup_v3 ON outhashes_v2 (method, outhash);

INSERT OR IGNORE INTO outhashes_v2 (method, taskhash, outhash)
	SELECT method, taskhash, outhash FROM outhashes;

-- Superseded by outhashes_v2 above; nothing reads or writes it from here on.
DROP TABLE IF EXISTS outhashes;

-- +goose Down
CREATE TABLE IF NOT EXISTS outhashes (
	method   TEXT NOT NULL,
	outhash  TEXT NOT NULL,
	taskhash TEXT NOT NULL,
	unihash  TEXT NOT NULL,
	PRIMARY KEY (method, outhash)
);
CREATE INDEX IF NOT EXISTS outhashes_by_taskhash ON outhashes (method, taskhash);

-- Rebuild from outhashes_v2: for each (method, outhash), take whichever
-- taskhash reported it earliest -- a NULL created (a row copied forward
-- above, rather than reported since) sorts first, which is right: it was
-- that output's only and earliest reporter -- then resolve that taskhash's
-- current unihash.
INSERT OR IGNORE INTO outhashes (method, outhash, taskhash, unihash)
	SELECT o.method, o.outhash, o.taskhash, u.unihash
	FROM outhashes_v2 o
	JOIN unihashes u ON u.method = o.method AND u.taskhash = o.taskhash
	WHERE o.id = (
		SELECT o2.id FROM outhashes_v2 o2
		WHERE o2.method = o.method AND o2.outhash = o.outhash
		ORDER BY o2.created, o2.id
		LIMIT 1
	);

DROP INDEX IF EXISTS outhash_lookup_v3;
DROP TABLE IF EXISTS outhashes_v2;
