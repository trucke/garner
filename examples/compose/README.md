# Run Garner with Docker Compose

This folder runs Garner with Prowlarr and SABnzbd on one machine with Docker Compose, and Jellyfin too if you want it. Garner works without Jellyfin, but then it does not wait for Jellyfin to confirm each import. To run Garner as a single container next to services you already have, follow [docs/install.md](../../docs/install.md) instead.

You need an x86 Linux machine with Docker and its Compose plugin, a Usenet provider and a Newznab indexer. Garner's image brings its own TheTVDB key.

All services see the media at `/srv/media`, as one folder: SABnzbd downloads into `media/usenet`, and Garner moves the files into `media/library` with a rename, which works only within one mount.

## Set it up

1. Download the files into a new folder:

   ```bash
   mkdir garner-stack && cd garner-stack
   curl -fsSL --remote-name-all "https://raw.githubusercontent.com/trucke/garner/main/examples/compose/{compose.yaml,garner.toml,.env.example}"
   ```

2. Create the folders and make UID 1000, which the services run as, their owner. Then create `.env` from the example and let only your user read it:

   ```bash
   mkdir -p garner media/usenet media/library/tv media/library/anime
   sudo chown -R 1000:1000 garner media
   cp .env.example .env
   chmod 600 .env
   ```

3. In `.env`, set your time zone. To run Jellyfin too, remove the `#` before `COMPOSE_PROFILES=jellyfin`.
4. In `garner.toml`, set `url` to the address clients reach this machine at, such as `https://nas.home.lan:7979`.
5. Start the services:

   ```bash
   docker compose up -d
   ```

   Garner starts without its API keys and reports the missing ones in `docker compose logs garner`.
6. Set up SABnzbd at `http://<machine>:8080`:
   1. Run its wizard with your Usenet provider.
   2. Under **Config › General**, set a username and password. Without them, any device on your network can open SABnzbd. Copy the API key from the same page to `SABNZBD_API_KEY` in `.env`.
   3. Under **Config › Folders**, set the temporary download folder to `/srv/media/usenet/incomplete` and the completed download folder to `/srv/media/usenet/complete`.
   4. Under **Config › Categories**, add the category `tv`.
7. Set up Prowlarr at `http://<machine>:9696`: choose a login, add your indexers and copy the API key from **Settings › General** to `PROWLARR_API_KEY` in `.env`.
8. If you run Jellyfin, set it up at `http://<machine>:8096`:
   1. Run its wizard and add a library of shows for `/srv/media/library/tv` and one for `/srv/media/library/anime`.
   2. Install the TheTVDB plugin and put it first in the libraries' metadata order, as step 1 of [Prepare the other services](../../docs/install.md#prepare-the-other-services) describes.
   3. Create an API key under **Dashboard › API Keys** and copy it to `JELLYFIN_API_KEY` in `.env`.
9. Start Garner again with the keys:

    ```bash
    docker compose up -d
    ```

10. Check that Garner runs:

    ```bash
    docker compose exec garner garnerd status
    ```

    The first line reads `● Garner <version> · running since …`.
11. Create a token for the device you use Garner from:

    ```bash
    docker compose exec garner garnerd token create laptop
    ```

    Install `garner` on that device as the [README](../../README.md#get-started) describes, run it and paste the connection code. Press `?`, then `a`, to open the About screen: it shows whether Prowlarr, SABnzbd, Jellyfin and TheTVDB answer.
12. Press `P` to see the profiles. Garner starts with Original language, which wants each show in its original audio. To download in another language, press `a` to start a profile from a shipped one, such as German or English, then `u` to make it the profile for new shows. [Choose which releases Garner accepts](../../README.md#choose-which-releases-garner-accepts) says more.

At start, Garner warns that the services' API keys travel over plain HTTP. Between containers on one machine, they never leave it.

## Back up and upgrade

`docker compose exec garner garnerd backup` writes a copy of the database to `garner/backups`. Run it daily, for example from a systemd timer.

To upgrade, check that Garner is idle, so the restart interrupts no import:

```bash
docker compose exec garner garnerd status --json
```

When `work.idle` is `true`, pull the new images and restart:

```bash
docker compose pull
docker compose up -d
```

## Other setups

- To keep the media elsewhere, replace `./media` in `compose.yaml` with that folder. Downloads and the library must stay below one folder.
- To use a Jellyfin you already run, leave `COMPOSE_PROFILES` unset. Set `url` in the `[jellyfin]` section of `garner.toml` to its address, and map the folders its libraries use with `paths`, as the comment there shows.
- On a host with SELinux, such as Fedora, add `:Z` to each volume of `compose.yaml` that one service uses, and `:z` to the `./media` volumes, which several services share.
- With rootless Podman, UID 1000 in the containers is not your user. Follow [docs/install.md](../../docs/install.md) instead.
