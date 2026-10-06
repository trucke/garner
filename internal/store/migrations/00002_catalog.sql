-- +goose Up
-- Shows Garner manages, as described by TVDB.
CREATE TABLE series (
    id INTEGER PRIMARY KEY, -- TVDB series ID
    slug TEXT NOT NULL,
    name TEXT NOT NULL, -- TVDB's primary name, usually the original title
    name_eng TEXT,
    name_deu TEXT,
    original_language TEXT NOT NULL,
    original_country TEXT NOT NULL,
    status TEXT NOT NULL,
    year INTEGER,
    first_aired TEXT,
    average_runtime INTEGER,
    default_order TEXT NOT NULL,
    tvdb_updated TEXT NOT NULL,
    added_at INTEGER NOT NULL,
    fetched_at INTEGER NOT NULL,
    xem_fetched_at INTEGER
) STRICT;

-- Episode orders TVDB offers for a show, such as aired, DVD or Netflix.
CREATE TABLE series_orders (
    series_id INTEGER NOT NULL REFERENCES series (id) ON DELETE CASCADE,
    type TEXT NOT NULL, -- official, dvd, absolute, alternate, regional, altdvd, alttwo
    name TEXT NOT NULL,
    alternate_name TEXT, -- such as "Netflix"
    PRIMARY KEY (series_id, type)
) STRICT;

-- Names releases may use for a show, from TVDB, TheXEM and reviewed
-- corrections shipped with Garner.
CREATE TABLE series_titles (
    id INTEGER PRIMARY KEY,
    series_id INTEGER NOT NULL REFERENCES series (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    language TEXT,
    season INTEGER, -- TVDB season the title stands for; NULL for the whole show
    source TEXT NOT NULL, -- tvdb, xem or garner
    scene_season INTEGER, -- TheXEM's season before translation to TVDB
    note TEXT
) STRICT;
CREATE INDEX series_titles_series ON series_titles (series_id);

CREATE TABLE episodes (
    id INTEGER PRIMARY KEY, -- TVDB episode ID
    series_id INTEGER NOT NULL REFERENCES series (id) ON DELETE CASCADE,
    aired TEXT,
    runtime INTEGER,
    absolute_number INTEGER,
    is_movie INTEGER NOT NULL,
    name TEXT, -- in the original language
    name_eng TEXT,
    name_deu TEXT
) STRICT;
CREATE INDEX episodes_series ON episodes (series_id);

-- An episode's season and number in each order.
CREATE TABLE episode_numbers (
    episode_id INTEGER NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
    series_id INTEGER NOT NULL,
    order_type TEXT NOT NULL,
    season INTEGER NOT NULL,
    number INTEGER NOT NULL,
    PRIMARY KEY (episode_id, order_type)
) STRICT;
CREATE INDEX episode_numbers_position ON episode_numbers (series_id, order_type, season, number);

-- TheXEM's mapping from TVDB aired order to scene numbering. episode_id is
-- NULL when TVDB has no episode at the mapped position.
CREATE TABLE scene_numbers (
    series_id INTEGER NOT NULL REFERENCES series (id) ON DELETE CASCADE,
    tvdb_season INTEGER NOT NULL,
    tvdb_episode INTEGER NOT NULL,
    tvdb_absolute INTEGER,
    scene_season INTEGER NOT NULL,
    scene_episode INTEGER NOT NULL,
    scene_absolute INTEGER,
    episode_id INTEGER REFERENCES episodes (id) ON DELETE SET NULL
) STRICT;
CREATE INDEX scene_numbers_series ON scene_numbers (series_id);

CREATE TABLE sync_state (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- +goose Down
DROP TABLE sync_state;
DROP TABLE scene_numbers;
DROP TABLE episode_numbers;
DROP TABLE episodes;
DROP TABLE series_titles;
DROP TABLE series_orders;
DROP TABLE series;
