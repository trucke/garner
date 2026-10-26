# Profiles

A profile says which releases Garner accepts for a show: the audio languages and resolutions, best first, and how Garner ranks and upgrades releases within them. A profile is a short TOML text, so you can share it and use profiles others wrote. [How Garner chooses releases](releases.md) lists the rules that apply under every profile.

## Format

```toml
#:schema https://raw.githubusercontent.com/trucke/garner/main/docs/profile.schema.json
garner_profile = 1
name = "German, English fallback"
description = "German audio; English when no German release exists"
audio = ["de", "en"]
resolution = ["1080p", "720p"]
priority = "audio"
dual_audio = "ignore"
unnamed_audio = "fallback"
upgrades = true
```

| Key | Required | Values | Meaning |
| --- | --- | --- | --- |
| `garner_profile` | yes | `1` | The format version. Garner refuses a profile of a newer version than it reads. |
| `name` | yes | text, up to 40 characters | The profile's name, unique within one Garner |
| `description` | no | one line, up to 200 characters | Shown in lists and before you import the profile |
| `audio` | yes | best first: `original`, `de`, `en`, `es`, `fr`, `hi`, `it`, `ja`, `ko`, `nl`, `pl`, `pt`, `ru`, `tr`, `zh` | The accepted audio languages. `original` is the show's original language. |
| `resolution` | yes | best first: `1080p`, `720p` | The accepted resolutions |
| `priority` | no, default `audio` | `audio`, `resolution` | Which list counts more when Garner ranks the preferences |
| `dual_audio` | no, default `prefer` | `prefer`, `ignore` | `prefer` ranks a release that also has the show's original audio first, when the preference's language is another one |
| `unnamed_audio` | no, default `reject` | `reject`, `fallback` | What to do with a release marked dual or multi that names no languages: reject it, or take it as a last resort and check it after download |
| `upgrades` | no, default `true` | `true`, `false` | Whether Garner replaces a file below the first preference when a release in a better preference appears |

Each language has one spelling: its two-letter code, such as `de` for German.

## Preferences

Garner combines every language of `audio` with every resolution of `resolution` into the profile's preferences, best first. With `priority = "audio"`, the language counts more:

1. German 1080p
2. German 720p
3. English 1080p
4. English 720p

With `priority = "resolution"`, the resolution counts more:

1. German 1080p
2. English 1080p
3. German 720p
4. English 720p

Garner downloads the release in the best preference it finds and rejects releases that meet no preference. With `upgrades = true`, it later replaces a file with a release in a better preference.

For each show, `original` becomes the show's original language. When that language is also in the list, Garner keeps only its first place: under `audio = ["original", "en"]`, an English show has the preferences English 1080p and English 720p.

## Check a profile

Garner checks every profile before it uses one, and lists every problem with the key it concerns and what would be right, for example `priority: "audio first" is not allowed; use audio or resolution`. It also warns about a setting that has no effect, such as `priority = "resolution"` with a single language.

To check a profile file without a server, run:

```bash
garner profile check german.toml
```

It lists the profile's preferences and settings, or every problem. `garner profile check -` reads the profile from standard input.

Editors check a profile while you type when they read its first line, `#:schema …`, as the TOML support in VS Code and other editors based on Taplo does. The schema is [`profile.schema.json`](profile.schema.json).

## Profiles that come with Garner

| Profile | `audio` | Other settings |
| --- | --- | --- |
| Original language | `original` | |
| German | `de` | |
| German, English fallback | `de`, `en` | `dual_audio = "ignore"`, `unnamed_audio = "fallback"` |
| English | `en` | |
| Original, English dub | `original`, `en` | |

All five accept `1080p` before `720p`. Their files are in [`internal/profile/shipped`](../internal/profile/shipped).
