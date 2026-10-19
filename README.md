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

`mise run check` checks the working copy: formatting, vet, the generated database code and known vulnerabilities; the tests on macOS and again on Linux in the Go image the container is built with; and builds for Linux x86. Run it before every push. The real-case tests from the private `garner-research` repository are not part of it; run them as `AGENTS.md` describes.

`mise run build` builds both programs without starting the server. `mise run generate` regenerates the database code after changes to `internal/store/*.sql` or the migrations.

## Releases

Versions follow semantic versioning: `v0.x.y` until Garner is ready for others.
`mise run release v0.2.0 [revision]` tags a commit of `main`, the newest by default, and pushes the
tag. The commit must be on `main` on GitHub and newer than the previous release; every push passed
`mise run check`, so it is checked. GitHub's release workflow (`.github/workflows/release.yml`)
then builds:

* the server image, `ghcr.io/trucke/garner:v0.2.0`, private while the repository is. Its build cache
  lives on `ghcr.io` too, so FFprobe is compiled only when the Dockerfile's FFmpeg changes;
* with GoReleaser (`.goreleaser.yaml`), the terminal interface for macOS (Apple silicon) and Linux
  (x86) with their SHA-256 sums, and the GitHub release with the image reference and the commits
  since the previous version.

`goreleaser release --snapshot --clean` runs GoReleaser's part locally into `dist/` without
publishing anything. The workflow keeps the twenty newest image versions, build cache included.

To install the terminal interface from a release, pick the file for the machine, `darwin-arm64` for a
Mac with Apple silicon or `linux-amd64` for Linux on x86:

```bash
mkdir -p ~/.local/bin
gh release download v0.2.0 -R trucke/garner -p 'garner-*-darwin-arm64' -O ~/.local/bin/garner
chmod +x ~/.local/bin/garner
```

## Container image

`mise run image` builds `localhost/garner:dev` with Podman from the `Dockerfile`. The image contains the server, `garnerd`, and an FFprobe that only reads MKV, MP4, AVI, MPEG-TS and WMV files.

| Item | Value |
| --- | --- |
| Command | `garnerd serve`; admin commands run with `podman exec <container> garnerd …` |
| User | UID and GID 1000 |
| Root filesystem | Can be read-only |
| `/config/garner.toml` | Configuration, read-only (`GARNER_CONFIG`) |
| `/data` | Database, backups, TLS key and the database engine's library, which `garnerd` unpacks on first start; set `data_dir = "/data"`. Use local disk, not a network share. Must be writable by UID 1000 in the container: with rootless Podman, run with `--userns=keep-id` so it maps to your own user, or set the owner with `podman unshare chown 1000:1000 <dir>` |
| Media | Mount the share at the paths in `library`, `sabnzbd.paths` and `jellyfin.paths` |
| Secrets | Environment variables or `*_FILE`, as in `garner.example.toml` |
| Port | 7979 |

## Files in the library

Garner manages one file per episode: the files it imports and the files it takes over. It leaves every other file in the library alone.

### Taking over files

When you add a show, Garner looks for the show's video files before the monitor starts on it. It looks in two places:

- Garner's folder for the show, such as `Harbor.Precinct.Night.Shift.2013.[tvdbid-42]`
- every folder that Jellyfin holds the show in, found by the show's TVDB ID, whatever the folder is called

Garner takes over a file when its name clearly names episodes that have no file yet. FFprobe reads the file's audio languages and resolution, which give the file its tier, as for a download. A file in a tier the profile does not accept, such as English audio for a TV show, ranks below every tier, so an upgrade replaces it. Of two files for one episode, Garner takes the one in the better tier, then the larger one. The terminal interface lists the files that Garner left alone, with the reason. To look again, for example after copying files into the library by hand, press `t` on the show's details page.

Garner reaches Jellyfin's folders only through `jellyfin.paths` in `garner.toml`. Map every folder that Jellyfin's libraries use, including the folders of a library from before Garner, and mount each one into Garner's container at the mapped path. When no mapping covers a folder, the terminal interface names the folder, and Garner looks only in its own folder.

### What taking over means for your files

A taken-over file belongs to Garner from then on, like a file it imported:

- An upgrade replaces the file and deletes it, with its subtitle files.
- Deleting the show with **Delete files** deletes the file. **Keep files** leaves it in place, and adding the show again takes it over again.
- The file keeps its name and folder. After a take-over, the terminal interface asks whether to rename and move the files outside Garner's folder into that folder, named the way Garner names files. **Keep as they are** is preselected.

Renaming moves the subtitle files along, never overwrites a file and tells Jellyfin about the moves. It removes the folders that the moves left empty. A folder that still holds other files, such as Jellyfin's `tvshow.nfo`, stays, and Jellyfin may still list it as a show. Garner moves files without copying them, so a file on another filesystem than Garner's folder stays where it is.

## Data sources

Metadata provided by [TheTVDB](https://thetvdb.com). Please consider adding missing information or subscribing.

Scene numbering and alternative titles come from [TheXEM](https://thexem.info). Reviewed corrections to that data live in `internal/catalog/corrections.toml`.
