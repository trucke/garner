-- +goose Up
-- Releases handed to SABnzbd. A row is written before SABnzbd is asked, so
-- after a crash Garner looks the job up instead of submitting it again.
CREATE TABLE downloads (
    id INTEGER PRIMARY KEY,
    episode_id INTEGER NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    indexer TEXT NOT NULL,
    link TEXT NOT NULL, -- holds Prowlarr's API key; never sent to clients
    size INTEGER NOT NULL,
    verdict TEXT NOT NULL,
    tier TEXT NOT NULL,
    manual INTEGER NOT NULL, -- chosen by hand rather than by Garner
    state TEXT NOT NULL, -- submitting, queued, downloading, processing, completed or failed
    detail TEXT NOT NULL, -- SABnzbd's own status, such as "Repairing"
    job_id TEXT UNIQUE, -- SABnzbd's nzo_id
    progress REAL NOT NULL, -- percent downloaded
    time_left TEXT NOT NULL,
    storage TEXT NOT NULL, -- where SABnzbd put the files, as SABnzbd sees its disk
    failure TEXT NOT NULL,
    retryable INTEGER NOT NULL, -- another release may succeed where this one failed
    stages TEXT NOT NULL, -- SABnzbd's post-processing log, JSON
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX downloads_episode ON downloads (episode_id);
CREATE INDEX downloads_state ON downloads (state);

-- +goose Down
DROP TABLE downloads;
