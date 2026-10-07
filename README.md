# Garner

Garner manages TV shows and anime for a self-hosted media library. It finds releases through Prowlarr, downloads them with SABnzbd, inspects the files and imports them for Jellyfin.

Garner is in early development and not ready for use.

## Run it locally

You need [mise](https://mise.jdx.dev) and the [Infisical CLI](https://infisical.com/docs/cli/overview) with access to the `media-manager` project.

1. Copy `garner.example.toml` to `garner.toml` and set the service URLs.
2. Run `mise run dev`. The task builds `bin/garner` and starts the server with development secrets from Infisical.
3. In a second terminal, run `./bin/garner token create <name>` to create an API token.

`mise run build` builds without starting the server. `mise run generate` regenerates the database code after changes to `internal/store/*.sql` or the migrations.

## Container image

`mise run image` builds `localhost/garner:dev` with Podman from the `Dockerfile`. The image contains Garner and an FFprobe that only reads MKV, MP4, AVI, MPEG-TS and WMV files.

| Item | Value |
| --- | --- |
| Command | `garner serve`; admin commands run with `podman exec <container> garner …` |
| User | UID and GID 1000 |
| Root filesystem | Can be read-only |
| `/config/garner.toml` | Configuration, read-only (`GARNER_CONFIG`) |
| `/data` | Database, backups and TLS key; set `data_dir = "/data"`. Use local disk, not a network share. Must be writable by UID 1000 in the container: with rootless Podman, run with `--userns=keep-id` so it maps to your own user, or set the owner with `podman unshare chown 1000:1000 <dir>` |
| Media | Mount the share at the paths in `library`, `sabnzbd.paths` and `jellyfin.paths` |
| Secrets | Environment variables or `*_FILE`, as in `garner.example.toml` |
| Port | 7979 |

## Data sources

Metadata provided by [TheTVDB](https://thetvdb.com). Please consider adding missing information or subscribing.

Scene numbering and alternative titles come from [TheXEM](https://thexem.info). Reviewed corrections to that data live in `internal/catalog/corrections.toml`.
