# Contributing to Garner

Bug reports, ideas and pull requests are welcome. For a larger change, open an issue first, so we can agree on the approach before you build it.

## Ground rules

- Develop and test against the development stack below, never against a library you care about.
- Keep real release names out of the repository: in tests, comments and commit messages, use made-up ones such as `Harbor.Precinct.S01E01.German.DL.1080p.WEB-GRP`.
- Run `mise run check` before you push. Every pull request runs the same checks on GitHub, without the macOS tests.
- Keep changes focused, with tests for the behaviour they change.

## Run it locally

Garner runs on your machine against a local development stack: SABnzbd, Prowlarr and Jellyfin in a Podman pod, so development never touches other services or libraries. You need [mise](https://mise.jdx.dev), [Podman](https://podman.io), a Usenet account, at least one Newznab indexer and a [TheTVDB API key](https://thetvdb.com/api-information).

1. Copy `devstack.example.toml` to `devstack.toml` and fill in your Usenet server and indexers. Set `TVDB_API_KEY` in the environment, for example in `mise.local.toml`:

   ```toml
   [env]
   TVDB_API_KEY = "..."
   ```

   Git ignores both files. A `devstack.toml` value written as `"${NAME}"` is read from that environment variable, so secrets can stay in a secret manager. `mise.local.toml` can also redefine the `dev` and `devstack` tasks to run through one.
2. Run `mise run devstack up`. It creates the stack in `~/garner-dev` and sets it up: SABnzbd with your Usenet server, Prowlarr with your indexers, and Jellyfin with the TheTVDB plugin and the TV and anime libraries. It also writes `garner.toml` for the stack, with the monitor turned off, so the development server searches and downloads only what you ask for. The first run downloads the images; later runs only bring the setup up to date.
3. Run `mise run dev`. The task builds the server, `bin/garnerd`, and starts it against the stack. [Air](https://github.com/air-verse/air) rebuilds and restarts it whenever the code changes; the terminal interface reconnects on its own.
4. In a second terminal, run `./bin/garnerd token create <name>` to create a token.
5. Run `mise run tui` to build the terminal interface, `bin/garner`, and open it. Paste the connection code from step 4. The terminal interface stores the connection in `~/.config/garner/connection`.

| Service | Address | Sign-in |
| --- | --- | --- |
| SABnzbd | <http://127.0.0.1:8080> | None |
| Prowlarr | <http://127.0.0.1:9696> | Set up on first visit |
| Jellyfin | <http://127.0.0.1:8096> | User `garner-dev`, password in `~/garner-dev/secrets/jellyfin_admin_password` |

The stack listens on `127.0.0.1` only. Downloads, the library, the settings and Garner's development database all live in `~/garner-dev`. The stack's own API keys are in `~/garner-dev/secrets`.

- `mise run devstack status` shows the containers.
- `mise run devstack down` stops them and keeps the data.
- `mise run devstack reset` deletes everything in `~/garner-dev` and builds the stack again. Stop Garner first.

`mise run devstack up` removes any Usenet server or indexer that is not in `devstack.toml`, so change them there rather than in SABnzbd or Prowlarr. Some providers and indexers limit how many places may use an account at once; give the stack an account, or an allowance, that nothing else needs.

## Check your changes

`mise run check` checks the working copy:

- formatting, `go vet`, the generated database code and known vulnerabilities
- the tests on macOS, and again on Linux in the Go image the container is built with
- a build for Linux on x86

Other tasks:

- `mise run build` builds both programs without starting the server.
- `mise run generate` regenerates the database code after changes to `internal/store/*.sql` or the migrations.
- `mise run image` builds the container image `localhost/garner:dev` with Podman.

### The simulation

`internal/simulation` runs Garner's server against a simulated SABnzbd, Prowlarr, Jellyfin and FFprobe, on a temporary share and a simulated clock. The share holds a library with files from before Garner, the user's own files and an older Jellyfin library; the downloads or the older library may sit on another disk. A seed drives every event: what the user does, how the services answer and fail, when a folder is unplugged or refuses writes, how the catalogue changes, when time jumps and when Garner restarts. The monitor runs too, with upgrades on. After every step, the test checks Garner's promises about files:

- Files no one gave Garner, such as the user's own files and Jellyfin's metadata, stay as they are.
- A video leaves the library only when a better file for its episodes replaced it, or when the user deleted its show with its files.
- Garner changes nothing outside the library and SABnzbd's folders, and removes a download's folder only when it is done with the download.
- Every file Garner records exists.
- Downloads and imports change state only along the allowed transitions.

After a calm period at the end, every download and every import must have ended, every wanted episode that aired must have a file unless it waits for the user, and Garner must have removed SABnzbd's folders of the downloads it imported.

`go test ./...` runs a fixed list of seeds, each twice: the second run must tell the same story. A failure names the seed, the step, the broken rule and the last steps, and ends with a command that replays it:

```bash
go test ./internal/simulation -run TestSimulation -seed 1842 -steps 317 -v
```

With `-v`, the replay prints every step. To explore, run `-seeds 1000` for a thousand fresh seeds instead of the fixed ones.

A failure is a bug in Garner or in the simulation. When you fix a bug it found, also add a focused test for it: after later changes, the same seed may take another path.

The fixed seeds were chosen so that, together, every event changes something and they catch bugs built into Garner on purpose. A change to the simulated world sends every seed down another path; the test then fails when an event no longer changes anything in any fixed seed. The comment on `fixedSeeds` says how to choose them again and lists the bugs to check by hand.

The maintainer also runs tests against real release names, which stay private. When `GARNER_MATCH_CASES`, `GARNER_SELECT_CASES` and `GARNER_RELEASE_CASES` point at such case files, `go test ./...` includes them; without them, those tests are skipped.

## How the code is organised

| Folder | Holds |
| --- | --- |
| `cmd/garnerd` | The server's commands: `serve`, `token`, `backup`, `status` |
| `cmd/garner` | The terminal interface's entry point |
| `internal/release` | Reading release names: episodes, resolution, source, languages |
| `internal/match` | Matching a release to a show's episodes, including scene and absolute numbering |
| `internal/selection` | Profiles, verdicts with their reasons, and ranking |
| `internal/search`, `internal/download`, `internal/importer`, `internal/monitor` | Searching through Prowlarr, downloading with SABnzbd, checking and importing files, and the monitor |
| `internal/catalog` | Show data from TheTVDB and TheXEM, with reviewed corrections in `corrections.toml` |
| `internal/store` | The database, its migrations and the `sqlc` queries |
| `internal/server`, `internal/api`, `internal/client` | The HTTP API, its JSON types and the client the terminal interface uses |
| `internal/tui` | The terminal interface; [docs/tui.md](docs/tui.md) describes its conventions |

## Releases

Maintainers release with `mise run release vX.Y.Z [revision]`. The task tags a commit of `main`, the newest by default, and pushes the tag. The commit must be on `main` on GitHub and newer than the previous release. GitHub's release workflow (`.github/workflows/release.yml`) then builds:

- the server image, `ghcr.io/trucke/garner:vX.Y.Z`. Its build cache lives on `ghcr.io` too, so FFprobe is compiled only when the Dockerfile's FFmpeg changes.
- with GoReleaser (`.goreleaser.yaml`), the terminal interface for macOS and Linux, on ARM and x86, with SHA-256 sums, and the GitHub release with the image reference and the commits since the previous version.

`goreleaser release --snapshot --clean` runs GoReleaser's part locally into `dist/` without publishing anything. The workflow keeps the twenty newest image versions, build cache included. Versions follow semantic versioning.
