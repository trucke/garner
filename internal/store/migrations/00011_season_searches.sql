-- +goose Up
-- The latest search for a whole season, kept for its packs: releases that
-- hold several of the season's episodes. Single episodes are searched one
-- by one.
CREATE TABLE season_searches (
    id INTEGER PRIMARY KEY,
    series_id INTEGER NOT NULL REFERENCES series (id) ON DELETE CASCADE,
    season INTEGER NOT NULL,
    searched_at INTEGER NOT NULL,
    outcome TEXT NOT NULL, -- found, fallback, nothing or failed
    summary TEXT NOT NULL,
    indexers TEXT NOT NULL, -- JSON: what each indexer returned
    UNIQUE (series_id, season)
) STRICT;

CREATE TABLE season_candidates (
    id INTEGER PRIMARY KEY,
    search_id INTEGER NOT NULL REFERENCES season_searches (id) ON DELETE CASCADE,
    -- The episode a download of the pack belongs to: its first missing one.
    episode_id INTEGER NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
    rank INTEGER NOT NULL,
    indexer TEXT NOT NULL,
    title TEXT NOT NULL,
    link TEXT NOT NULL, -- holds Prowlarr's API key; never sent to clients
    size INTEGER NOT NULL,
    published INTEGER,
    verdict TEXT NOT NULL,
    tier TEXT NOT NULL,
    audio TEXT NOT NULL, -- language codes, comma-separated
    reasons TEXT NOT NULL, -- JSON array
    decisive INTEGER NOT NULL,
    episodes TEXT NOT NULL -- JSON array of TVDB episode IDs
) STRICT;
CREATE INDEX season_candidates_search ON season_candidates (search_id);

-- +goose Down
DROP TABLE season_candidates;
DROP TABLE season_searches;
