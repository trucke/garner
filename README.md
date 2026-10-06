# Garner

Garner manages TV shows and anime for a self-hosted media library. It finds releases through Prowlarr, downloads them with SABnzbd, inspects the files and imports them for Jellyfin.

Garner is in early development and not ready for use.

## Run it locally

You need [mise](https://mise.jdx.dev) and the [Infisical CLI](https://infisical.com/docs/cli/overview) with access to the `media-manager` project.

1. Copy `garner.example.toml` to `garner.toml` and set the service URLs.
2. Run `mise run dev`. The task builds `bin/garner` and starts the server with development secrets from Infisical.
3. In a second terminal, run `./bin/garner token create <name>` to create an API token.

`mise run build` builds without starting the server. `mise run generate` regenerates the database code after changes to `internal/store/*.sql` or the migrations.

## Data sources

Metadata provided by [TheTVDB](https://thetvdb.com). Please consider adding missing information or subscribing.

Scene numbering and alternative titles come from [TheXEM](https://thexem.info). Reviewed corrections to that data live in `internal/catalog/corrections.toml`.
