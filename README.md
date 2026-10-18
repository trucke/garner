# Garner

Garner manages TV shows and anime for a self-hosted media library. It finds releases through Prowlarr, downloads them with SABnzbd, inspects the files and imports them for Jellyfin.

Garner is in early development and not ready for use.

## Run it locally

Garner runs on your machine against a local development stack: SABnzbd, Prowlarr and Jellyfin in a Podman pod, so development never touches the production services or library. You need [mise](https://mise.jdx.dev), [Podman](https://podman.io) and the [Infisical CLI](https://infisical.com/docs/cli/overview) with access to the `garner` project.

1. Run `mise run devstack up`. It creates the stack in `~/garner-dev` and sets it up: SABnzbd with your Usenet server, Prowlarr with the indexers, and Jellyfin with the TVDB plugin and the TV and Anime libraries. It also writes `garner.toml` for the stack, with the monitor turned off: the development server searches and downloads only what you ask for. The first run downloads the images; later runs only bring the setup up to date.
2. Run `mise run dev`. The task builds the server, `bin/garnerd`, and starts it against the stack. [Air](https://github.com/air-verse/air) rebuilds and restarts it whenever the code changes; the terminal interface reconnects on its own.
3. In a second terminal, run `./bin/garnerd token create <name>` to create an API token.
4. Run `mise run tui` to build `bin/garner`, the terminal interface, and open it, and paste the connection code from step 3. It stores the connection in `~/.config/garner/connection`.

| Service | Address | Sign-in |
| --- | --- | --- |
| SABnzbd | <http://127.0.0.1:8080> | None |
| Prowlarr | <http://127.0.0.1:9696> | Set up on first visit |
| Jellyfin | <http://127.0.0.1:8096> | User `garner-dev`, password in `~/garner-dev/secrets/jellyfin_admin_password` |

The stack listens on `127.0.0.1` only. Downloads, the library, the settings and Garner's development database all live in `~/garner-dev`.

* `mise run devstack status` shows the containers.
* `mise run devstack down` stops them and keeps the data.
* `mise run devstack reset` deletes everything in `~/garner-dev` and builds the stack again. Stop Garner first.

Infisical (env `dev`) supplies only external credentials: `TVDB_API_KEY`, `USENET_USERNAME`, `USENET_PASSWORD`, `INDEXER_API_KEY` and `SECOND_INDEXER_API_KEY`. The stack's own API keys stay in `~/garner-dev/secrets`.

The development stack downloads from its own Usenet account with 8 connections. Use an account that nothing else uses: some providers block accounts that are used from several places at once.

The second indexer allows API use from two devices: production Prowlarr and the development stack's Prowlarr. Never set up a third Prowlarr with it.

`mise run check` checks the working copy: formatting, vet, the generated database code and known vulnerabilities; the tests on macOS and again on Linux in the Go image the container is built with; and builds for Linux x86. A pass is recorded as a git note (`refs/notes/garner-check`) on the working copy's tree. The real-case tests from the private `garner-research` repository are not part of it; run them as `AGENTS.md` describes.

`mise run build` builds both programs without starting the server. `mise run generate` regenerates the database code after changes to `internal/store/*.sql` or the migrations.

## Container image

`mise run image` builds `localhost/garner:dev` with Podman from the `Dockerfile`. The image contains the server, `garnerd`, and an FFprobe that only reads MKV, MP4, AVI, MPEG-TS and WMV files.

| Item | Value |
| --- | --- |
| Command | `garnerd serve`; admin commands run with `podman exec <container> garnerd …` |
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
