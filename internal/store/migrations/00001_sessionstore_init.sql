-- +goose Up
CREATE SCHEMA IF NOT EXISTS rubbish;

CREATE TABLE rubbish.sessions (
    id         TEXT PRIMARY KEY,
    slot       INTEGER NOT NULL,
    status     TEXT NOT NULL,
    repo_url   TEXT NOT NULL DEFAULT '',
    branch     TEXT NOT NULL DEFAULT '',
    error_msg  TEXT NOT NULL DEFAULT '',
    dev_mode   BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    role       TEXT NOT NULL DEFAULT '',
    prompt     TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE rubbish.favorites (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    repo_url   TEXT NOT NULL,
    branch     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE rubbish.favorites;
DROP TABLE rubbish.sessions;
DROP SCHEMA rubbish;
