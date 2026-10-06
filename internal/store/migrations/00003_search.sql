-- +goose Up
-- The show's profile, tv or anime. NULL until a refresh sets it from TVDB's
-- genres; later refreshes keep it, so a changed profile stays.
ALTER TABLE series ADD COLUMN profile TEXT;

-- The latest search for an episode.
CREATE TABLE searches (
    id INTEGER PRIMARY KEY,
    episode_id INTEGER NOT NULL UNIQUE REFERENCES episodes (id) ON DELETE CASCADE,
    searched_at INTEGER NOT NULL,
    outcome TEXT NOT NULL, -- found, fallback, nothing or failed
    summary TEXT NOT NULL,
    next_retry_at INTEGER
) STRICT;

-- What each indexer returned, kept apart so failures are not mistaken for
-- empty results.
CREATE TABLE search_indexers (
    search_id INTEGER NOT NULL REFERENCES searches (id) ON DELETE CASCADE,
    indexer TEXT NOT NULL,
    results INTEGER NOT NULL,
    error TEXT
) STRICT;
CREATE INDEX search_indexers_search ON search_indexers (search_id);

-- Releases a search found, best first.
CREATE TABLE candidates (
    id INTEGER PRIMARY KEY,
    search_id INTEGER NOT NULL REFERENCES searches (id) ON DELETE CASCADE,
    rank INTEGER NOT NULL,
    indexer TEXT NOT NULL,
    title TEXT NOT NULL,
    link TEXT NOT NULL, -- holds Prowlarr's API key; never sent to clients
    size INTEGER NOT NULL,
    published INTEGER,
    grabs INTEGER NOT NULL,
    verdict TEXT NOT NULL, -- accept, fallback or reject
    tier TEXT NOT NULL,
    audio TEXT NOT NULL, -- language codes, comma-separated
    reasons TEXT NOT NULL, -- JSON array
    episodes TEXT NOT NULL -- JSON array of TVDB episode IDs
) STRICT;
CREATE INDEX candidates_search ON candidates (search_id);

-- +goose Down
DROP TABLE candidates;
DROP TABLE search_indexers;
DROP TABLE searches;
ALTER TABLE series DROP COLUMN profile;
