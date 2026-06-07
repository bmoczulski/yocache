-- +goose Up
CREATE TABLE IF NOT EXISTS unihashes (
	method   TEXT NOT NULL,
	taskhash TEXT NOT NULL,
	unihash  TEXT NOT NULL,
	PRIMARY KEY (method, taskhash)
);
CREATE INDEX IF NOT EXISTS unihashes_by_unihash ON unihashes (unihash);
CREATE TABLE IF NOT EXISTS outhashes (
	method   TEXT NOT NULL,
	outhash  TEXT NOT NULL,
	taskhash TEXT NOT NULL,
	unihash  TEXT NOT NULL,
	PRIMARY KEY (method, outhash)
);

-- +goose Down
DROP INDEX IF EXISTS unihashes_by_unihash;
DROP TABLE IF EXISTS unihashes;
DROP TABLE IF EXISTS outhashes;
