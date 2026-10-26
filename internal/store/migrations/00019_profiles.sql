-- +goose Up
-- Profiles say which releases a show accepts. Each keeps its text in the
-- format users share (docs/profiles.md); name repeats the name in the
-- text, so names stay unique. New TV shows get the profile marked
-- default_tv, new anime shows the one marked default_anime: one each.
CREATE TABLE profiles (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    text TEXT NOT NULL,
    default_tv INTEGER NOT NULL DEFAULT 0,
    default_anime INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE UNIQUE INDEX profiles_default_tv ON profiles (default_tv) WHERE default_tv = 1;
CREATE UNIQUE INDEX profiles_default_anime ON profiles (default_anime) WHERE default_anime = 1;

-- A show's type, tv or anime, decides how Garner reads episode numbers and
-- which library folder takes its files; its profile which releases it
-- accepts. Both stay NULL until a refresh loads the show from TVDB, as the
-- profile did.
ALTER TABLE series ADD COLUMN type TEXT;
ALTER TABLE series ADD COLUMN profile_id INTEGER REFERENCES profiles (id);
UPDATE series SET type = profile;

-- Shows from before keep their rules: the built-in tv and anime profiles
-- become "German" and "German, English fallback", the defaults for their
-- types. A new installation starts with "Original language" for both.
-- +goose StatementBegin
INSERT INTO profiles (name, text, default_tv) SELECT 'German', '#:schema https://raw.githubusercontent.com/trucke/garner/main/docs/profile.schema.json
# A Garner profile; https://github.com/trucke/garner/blob/main/docs/profiles.md describes the format.
garner_profile = 1
name = "German"
description = "German audio; releases with the original audio too rank first"
audio = ["de"]
resolution = ["1080p", "720p"]
priority = "audio"
dual_audio = "prefer"
unnamed_audio = "reject"
upgrades = true
', 1
WHERE EXISTS (SELECT 1 FROM series);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO profiles (name, text, default_anime) SELECT 'German, English fallback', '#:schema https://raw.githubusercontent.com/trucke/garner/main/docs/profile.schema.json
# A Garner profile; https://github.com/trucke/garner/blob/main/docs/profiles.md describes the format.
garner_profile = 1
name = "German, English fallback"
description = "German audio; English when no German release exists"
audio = ["de", "en"]
resolution = ["1080p", "720p"]
priority = "audio"
dual_audio = "ignore"
unnamed_audio = "fallback"
upgrades = true
', 1
WHERE EXISTS (SELECT 1 FROM series);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO profiles (name, text, default_tv, default_anime) SELECT 'Original language', '#:schema https://raw.githubusercontent.com/trucke/garner/main/docs/profile.schema.json
# A Garner profile; https://github.com/trucke/garner/blob/main/docs/profiles.md describes the format.
garner_profile = 1
name = "Original language"
description = "Each show in its original language"
audio = ["original"]
resolution = ["1080p", "720p"]
priority = "audio"
dual_audio = "prefer"
unnamed_audio = "reject"
upgrades = true
', 1, 1
WHERE NOT EXISTS (SELECT 1 FROM series);
-- +goose StatementEnd
UPDATE series SET profile_id = (
    SELECT id FROM profiles WHERE name = CASE series.profile WHEN 'anime' THEN 'German, English fallback' ELSE 'German' END
) WHERE profile IS NOT NULL;
ALTER TABLE series DROP COLUMN profile;

-- +goose Down
-- SQLite cannot drop a column that references another table, so type and
-- profile_id stay, unused by the older Garner.
ALTER TABLE series ADD COLUMN profile TEXT;
UPDATE series SET profile = type, profile_id = NULL;
DROP TABLE profiles;
