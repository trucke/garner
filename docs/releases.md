# How Garner chooses releases

Garner judges every release a search finds by the show's profile and by the rules on this page. The profile sets the preferences: the audio languages and resolutions the show accepts, best first. [Profiles](profiles.md) describes the profiles and their settings. The rules on this page apply under every profile. Apart from the preferences, `dual_audio` and `unnamed_audio`, no setting changes them.

## Verdicts

Every release gets one of three verdicts, with the reasons for it. The terminal interface shows them in every search.

| Verdict | What Garner does |
| --- | --- |
| Accepted | The monitor downloads the best accepted release on its own. |
| Last resort | Garner downloads it only when you choose it, when you ask for the best release of an episode and no accepted release is left to try, or to replace a failed release that was not accepted either. |
| Rejected | Garner downloads it only when you choose it. |

Indexers often list a release more than once under the same name: separate uploads of it, which can differ in whether they still download completely. Garner tells them apart by the indexer's ID for each upload. On its own, it never picks an upload that already failed or was cancelled for the episode, not even after a new search, but another upload of the same release stays in the running. You can still choose any of them.

In the terminal interface, the Tried column of the releases list shows what came of each upload Garner tried, and WHY shows when the selected one was uploaded.

## Always rejected

Garner rejects a release when:

- it cannot tell which episode the release holds
- the release does not hold the episode searched for
- the name marks extra content instead of a regular episode
- the indexer files the release under another show
- the release is a remux, an unchanged copy of a disc
- its source is a DVD or a TV recording in standard definition
- the name states one resolution, and it is below 720p or 2160p or higher
- its audio is in none of the profile's languages
- no preference of the profile has its resolution in its audio language
- in an episode search, the library has the episode in the same or a better preference already. A better source alone, such as Blu-ray instead of web, does not replace a file. A season search judges packs without this rule. When Garner imports a pack, the pack's other episodes keep their file if it meets the same or a better preference.

## Only a last resort

A release is only a last resort when:

- the name states no resolution, or two different ones
- the profile has `unnamed_audio = "fallback"`, neither the indexer nor the name names an audio language, the name marks the audio as dual, multi or dubbed, and the show's original language is unknown or not among the profile's languages. With `unnamed_audio = "reject"`, such a release is rejected.

## The audio of a release

Garner takes a release's audio languages from the first of these that names any:

1. the languages the indexer reports
2. the languages the release's name states
3. the show's original language, because release names usually leave it out

A release whose audio language Garner assumed ranks behind one whose language is named. The file check before import confirms the language.

## Order

Garner ranks the releases of a search by these rules. Each rule decides only between releases that the rules before it rank equal:

1. Accepted releases before last resorts, and last resorts before rejected releases.
2. The better preference of the profile. A last resort without a known resolution counts as the best preference its audio language allows.
3. A release far larger than its resolution needs goes after the others: above 150 MB per minute of runtime at 1080p, or above 75 MB per minute at 720p. Garner compares only when the size, the runtime and one resolution are known.
4. When several aired episodes of the season are missing, a pack that holds all of them.
5. A release that holds exactly the searched episode, without doubt, before packs and uncertain matches.
6. A release whose audio language is named before one whose language Garner assumed.
7. Under a profile with `dual_audio = "prefer"`, a release that also has the show's original audio, when the preference's language is another one.
8. The source: WEB-DL first, then WEBRip and releases that only say WEB, then Blu-ray, then HDTV and HDRip. A release that names no source comes last.
9. More fixes first: each PROPER or REPACK counts one, and a version such as v2 counts its number.
10. The newest release.

## The file check before import

Garner imports the video files of a download. It leaves out samples and files far smaller than the largest one. When a download holds several videos, it also leaves out those it cannot assign to an episode, and those of other episodes whose file in the library meets the same or a better preference. FFprobe then reads each remaining video, and Garner rejects the download when:

- the download holds no video file, or no video of the episode the release was downloaded for
- FFprobe cannot read a video
- a video has no video track or no audio track
- the video is below 720p, or 2160p or higher. Garner reads the resolution as releases name it, from the width or the height, so a widescreen 1920×800 video counts as 1080p.
- the preference the release meets is 1080p, but the video is smaller
- no audio track is in the language of the preference the release meets, or for a last resort, in one of the profile's languages. When no audio track carries a language tag Garner knows, the video passes. For a release you chose by hand, a missing language is only noted.
- a video runs longer than one and a half times the episodes' runtime plus five minutes, or shorter than half of it. Garner checks the length only when TheTVDB lists a runtime and FFprobe reports the video's duration.

When Garner cannot read SABnzbd's folder or FFprobe does not run, the import stops instead, until you fix the problem and retry it.

## After a failed download

Garner downloads another release when a download fails in a way another release may avoid: the file check rejected it, or SABnzbd reports a problem of the release itself, such as missing articles, a failed repair or a password. A failure of the disk, of permissions or one Garner does not recognize starts no other release.

- Garner deletes the rejected download's folder when it can tell that the folder belongs to that download.
- It takes the best release left in the latest results of the same search, the episode's or the season's, and skips uploads that failed or were cancelled before. Another upload of the failed release is not skipped.
- An accepted release is replaced by accepted releases only, which may meet a lower preference. After a release that was not accepted, a last resort may follow.
- After the fourth failed download of the episode since that search, including downloads you chose by hand, Garner stops and leaves the choice to you.

## TV and anime

Every show has a type, TV or anime. A show in TheTVDB's genre Anime gets the type anime when you add it, and every new show gets the profile its type has as the default. To change a show's type or profile, press `p` on its details page. The type decides two things:

- **Numbering.** When a number such as 101 could mean season 1, episode 1 or episode 101, anime reads it as episode 101 first, and TV as season 1, episode 1.
- **Library folder.** New files of anime shows go to `library.anime`, those of TV shows to `library.tv`. Changing the type does not move files already in the library.

TheXEM's scene numbering applies to every show.
