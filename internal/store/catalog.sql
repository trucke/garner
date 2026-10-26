-- The monitor mode is chosen when adding a show; refreshes keep it, as
-- they keep a changed type and profile.
-- name: UpsertSeries :exec
INSERT INTO series (id, slug, name, name_eng, name_deu, original_language, original_country, status, year,
    first_aired, average_runtime, default_order, tvdb_updated, added_at, fetched_at, type, profile_id,
    network, last_aired, overview, overview_language, monitor, monitor_since)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    type = coalesce(series.type, excluded.type), profile_id = coalesce(series.profile_id, excluded.profile_id),
    slug = excluded.slug, name = excluded.name, name_eng = excluded.name_eng, name_deu = excluded.name_deu,
    original_language = excluded.original_language, original_country = excluded.original_country,
    status = excluded.status, year = excluded.year, first_aired = excluded.first_aired,
    average_runtime = excluded.average_runtime, default_order = excluded.default_order,
    tvdb_updated = excluded.tvdb_updated, fetched_at = excluded.fetched_at,
    network = excluded.network, last_aired = excluded.last_aired, overview = excluded.overview,
    overview_language = excluded.overview_language;

-- name: SetXEMFetched :exec
UPDATE series SET xem_fetched_at = ? WHERE id = ?;

-- name: SeriesExists :one
SELECT count(*) FROM series WHERE id = ?;

-- name: GetSeries :one
SELECT * FROM series WHERE id = ?;

-- name: ListSeries :many
SELECT * FROM series ORDER BY coalesce(name_eng, name);

-- name: ListSeriesIDs :many
SELECT id FROM series ORDER BY id;

-- name: DeleteSeriesOrders :exec
DELETE FROM series_orders WHERE series_id = ?;

-- name: InsertSeriesOrder :exec
INSERT INTO series_orders (series_id, type, name, alternate_name) VALUES (?, ?, ?, ?);

-- name: ListSeriesOrders :many
SELECT type, name, alternate_name FROM series_orders WHERE series_id = ? ORDER BY type;

-- name: DeleteSeriesTitles :exec
DELETE FROM series_titles WHERE series_id = ?;

-- name: InsertSeriesTitle :exec
INSERT INTO series_titles (series_id, title, language, season, source, scene_season, note) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListSeriesTitles :many
SELECT title, language, season, source, scene_season, note FROM series_titles WHERE series_id = ? ORDER BY source, season, title;

-- name: UpsertEpisode :exec
INSERT INTO episodes (id, series_id, aired, runtime, absolute_number, is_movie, name, name_eng, name_deu, overview,
    overview_language)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    series_id = excluded.series_id, aired = excluded.aired, runtime = excluded.runtime,
    absolute_number = excluded.absolute_number, is_movie = excluded.is_movie, name = excluded.name,
    name_eng = excluded.name_eng, name_deu = excluded.name_deu, overview = excluded.overview,
    overview_language = excluded.overview_language;

-- name: ListEpisodeIDs :many
SELECT id FROM episodes WHERE series_id = ?;

-- name: DeleteEpisode :exec
DELETE FROM episodes WHERE id = ?;

-- Without overviews: a show's episodes load often, their descriptions are
-- read one at a time.
-- name: ListEpisodes :many
SELECT id, series_id, aired, runtime, absolute_number, is_movie, name, name_eng, name_deu
FROM episodes WHERE series_id = ? ORDER BY id;

-- name: GetEpisodeOverview :one
SELECT overview, overview_language FROM episodes WHERE id = ?;

-- name: DeleteEpisodeNumbers :exec
DELETE FROM episode_numbers WHERE series_id = ?;

-- name: InsertEpisodeNumber :exec
INSERT INTO episode_numbers (episode_id, series_id, order_type, season, number) VALUES (?, ?, ?, ?, ?);

-- name: ListEpisodeNumbers :many
SELECT episode_id, order_type, season, number FROM episode_numbers WHERE series_id = ?;

-- name: DeleteSceneNumbers :exec
DELETE FROM scene_numbers WHERE series_id = ?;

-- name: InsertSceneNumber :exec
INSERT INTO scene_numbers (series_id, tvdb_season, tvdb_episode, tvdb_absolute, scene_season, scene_episode, scene_absolute, episode_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListSceneNumbers :many
SELECT tvdb_season, tvdb_episode, tvdb_absolute, scene_season, scene_episode, scene_absolute, episode_id
FROM scene_numbers WHERE series_id = ? ORDER BY tvdb_season, tvdb_episode;

-- name: GetSyncState :one
SELECT value FROM sync_state WHERE key = ?;

-- name: SetSyncState :exec
INSERT INTO sync_state (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value;
