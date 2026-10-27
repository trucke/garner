package api

import "time"

// Series is a show in the catalogue.
type Series struct {
	ID               int64  `json:"id"`
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	NameEng          string `json:"name_eng,omitempty"`
	NameDeu          string `json:"name_deu,omitempty"`
	OriginalLanguage string `json:"original_language"`
	OriginalCountry  string `json:"original_country"`
	Status           string `json:"status"`
	Year             int    `json:"year,omitempty"`
	FirstAired       string `json:"first_aired,omitempty"`
	AverageRuntime   int    `json:"average_runtime,omitempty"`
	DefaultOrder     string `json:"default_order"`
	// Type is TypeTV or TypeAnime: how Garner reads episode numbers and
	// which library folder takes the show's files. ProfileID is the
	// profile that says which releases the show accepts.
	Type        string     `json:"type"`
	ProfileID   int64      `json:"profile_id"`
	TVDBUpdated string     `json:"tvdb_updated"`
	Added       time.Time  `json:"added"`
	Fetched     time.Time  `json:"fetched"`
	XEMFetched  *time.Time `json:"xem_fetched,omitempty"`
	// TVDB facts for the details page. The overview is in English when TVDB
	// has it, else German, else the original language; OverviewLanguage
	// says which.
	Network          string `json:"network,omitempty"`
	LastAired        string `json:"last_aired,omitempty"`
	Overview         string `json:"overview,omitempty"`
	OverviewLanguage string `json:"overview_language,omitempty"`
	// HasDetails is false for a show loaded before those facts were kept.
	HasDetails bool `json:"has_details"`
	// Monitor is which episodes the monitor gets: MonitorNew, MonitorAll or
	// MonitorOff. New episodes are those airing from MonitorSince on.
	Monitor      string    `json:"monitor"`
	MonitorSince time.Time `json:"monitor_since"`
}

// Monitors reports whether the monitor gets the show's episode that airs on
// aired, a date.
func (sr Series) Monitors(aired string) bool {
	switch sr.Monitor {
	case MonitorAll:
		return true
	case MonitorNew:
		return aired != "" && aired >= sr.MonitorSince.Local().Format(time.DateOnly)
	}
	return false
}

// Show types: how Garner reads a show's episode numbers and which library
// folder takes its files.
const (
	TypeTV    = "tv"
	TypeAnime = "anime" // reads a bare number such as 101 as an absolute episode first
)

// Monitor modes: which of a show's episodes the monitor gets on its own.
const (
	MonitorNew = "new" // those airing from the day the mode was chosen
	MonitorAll = "all" // every aired episode not in the library
	MonitorOff = "off" // none; Garner searches when asked
)

// MonitorModes lists the modes, the default first.
var MonitorModes = []string{MonitorNew, MonitorAll, MonitorOff}

// Order is an episode order TVDB offers, such as aired or Netflix.
type Order struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	AlternateName string `json:"alternate_name,omitempty"`
}

// Title is a name releases may use for a show.
type Title struct {
	Title    string `json:"title"`
	Language string `json:"language,omitempty"`
	// Season is the TVDB season the title stands for; nil means the whole show.
	Season *int `json:"season,omitempty"`
	// Source is tvdb, xem or garner (a reviewed correction).
	Source      string `json:"source"`
	SceneSeason *int   `json:"scene_season,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Position is a season and episode number in one order.
type Position struct {
	Season int `json:"season"`
	Number int `json:"number"`
}

// Episode is a TVDB episode with its position in every order.
type Episode struct {
	ID       int64  `json:"id"`
	Aired    string `json:"aired,omitempty"`
	Runtime  int    `json:"runtime,omitempty"`
	Absolute int    `json:"absolute,omitempty"`
	IsMovie  bool   `json:"is_movie,omitempty"`
	Name     string `json:"name,omitempty"`
	NameEng  string `json:"name_eng,omitempty"`
	NameDeu  string `json:"name_deu,omitempty"`
	// Overview is TVDB's description in OverviewLanguage. Saved with the
	// show, but read only through EpisodeOverview and never sent with it.
	Overview         string `json:"-"`
	OverviewLanguage string `json:"-"`
	// Numbers maps an order type to the episode's position in it.
	Numbers map[string]Position `json:"numbers"`
}

// SceneNumber maps a TVDB aired-order position to scene numbering.
type SceneNumber struct {
	TVDB          Position `json:"tvdb"`
	TVDBAbsolute  int      `json:"tvdb_absolute,omitempty"`
	Scene         Position `json:"scene"`
	SceneAbsolute int      `json:"scene_absolute,omitempty"`
	// EpisodeID is nil when TVDB has no episode at the mapped position.
	EpisodeID *int64 `json:"episode_id,omitempty"`
}

// SeriesData is a show with everything Garner knows about it.
type SeriesData struct {
	Series   Series        `json:"series"`
	Orders   []Order       `json:"orders"`
	Titles   []Title       `json:"titles"`
	Episodes []Episode     `json:"episodes"`
	Scene    []SceneNumber `json:"scene_numbers"`
}

// Episode states, most urgent first. Shows take the state of their most
// urgent episode.
const (
	EpisodeFailed      = "failed"       // the latest download or import failed and nothing replaced it
	EpisodeAttention   = "attention"    // in the library, but Jellyfin did not confirm it
	EpisodeDownloading = "downloading"  // with SABnzbd, waiting or downloading
	EpisodeProcessing  = "processing"   // SABnzbd repairs or unpacks it
	EpisodeImporting   = "importing"    // file check, move or waiting for Jellyfin
	EpisodeWaiting     = "waiting"      // aired and missing; searched without a usable result
	EpisodeNotSearched = "not_searched" // aired and missing; never searched
	EpisodeUpcoming    = "upcoming"     // not aired yet
	EpisodeImported    = "imported"     // in the library
)

// Show is a show with a summary of what Garner has of it.
type Show struct {
	Series
	// Aired counts the episodes aired so far, specials excluded; Imported
	// counts those in the library.
	Aired    int `json:"aired"`
	Imported int `json:"imported"`
	// Bytes is the size of the imported files, each file counted once.
	Bytes int64 `json:"bytes"`
	// NextAired is the air date of the next episode, specials excluded.
	NextAired string `json:"next_aired,omitempty"`
	// Counts holds how many episodes are in each state other than imported
	// and upcoming, by state name.
	Counts map[string]int `json:"counts"`
	// Focus is the most urgent episode; nil when every aired episode is in
	// the library and none is upcoming.
	Focus *Focus `json:"focus,omitempty"`
}

// Focus is the episode a show's state comes from.
type Focus struct {
	EpisodeID int64  `json:"episode_id"`
	Season    int    `json:"season"`
	Number    int    `json:"number"`
	State     string `json:"state"`
	Aired     string `json:"aired,omitempty"`
	// Download is the episode's latest download, when it has one.
	Download *Download `json:"download,omitempty"`
	// Summary comes from the episode's latest search; NextRetry is when
	// the monitor searches it again, unset when it does not get it.
	Summary   string     `json:"summary,omitempty"`
	NextRetry *time.Time `json:"next_retry,omitempty"`
}

// EpisodeStatus is what Garner has of one episode.
type EpisodeStatus struct {
	EpisodeID int64 `json:"episode_id"`
	// State is one of the episode states, or empty for a special nobody
	// asked for.
	State string `json:"state"`
	// Size and Tier describe the imported file: its size and the tier of
	// the release it came from, such as "German 1080p".
	Size int64  `json:"size,omitempty"`
	Tier string `json:"tier,omitempty"`
	// Attempts counts the downloads started for the episode.
	Attempts int `json:"attempts,omitempty"`
	// Download is the latest download when it explains the state.
	Download *Download `json:"download,omitempty"`
	// Summary comes from the latest search; NextRetry is when the monitor
	// searches again, unset when it does not get the episode.
	Summary   string     `json:"summary,omitempty"`
	NextRetry *time.Time `json:"next_retry,omitempty"`
}

// SeriesDetail is a show with everything Garner knows about it, the
// problems found in imported data, and what Garner has of each episode.
type SeriesDetail struct {
	SeriesData
	Issues []Issue `json:"issues"`
	// Summary counts the show's episodes as the library does.
	Summary Show `json:"summary"`
	// States holds every episode's state; Best lists the tiers no upgrade
	// can beat, so clients can mark files below them.
	States []EpisodeStatus `json:"states"`
	Best   string          `json:"best_tier"`
	// Folder is where new files of the show go; URL is its TVDB page.
	Folder string `json:"folder,omitempty"`
	URL    string `json:"url"`
	// TypeFolders maps TypeTV and TypeAnime to the library folders new
	// files of each type go to, as the server sees them; only the folders
	// that are set.
	TypeFolders map[string]string `json:"type_folders"`
	// Profile names the show's profile and Rules describes it; Profiles
	// lists the profiles to choose from, with their rules for the show.
	Profile  string        `json:"profile"`
	Rules    string        `json:"rules"`
	Profiles []ProfileInfo `json:"profiles"`
	// LanguageName and LanguageNote say what the original language means
	// for the audio.
	LanguageName string `json:"language_name"`
	LanguageNote string `json:"language_note"`
	// MonitorOff says why the monitor does not run, whatever the show's
	// mode; empty when it runs.
	MonitorOff string `json:"monitor_off,omitempty"`
	// Searching is the search of every missing episode, while it is asked
	// for or runs.
	Searching *ShowProgress `json:"searching,omitempty"`
	// TakenOver is what adding the show found in the library; only the
	// answer to adding a show has it.
	TakenOver *TakeOver `json:"taken_over,omitempty"`
}

// TakeOver is what Garner found when it looked for a show's files in the
// library: in its own folder for the show and the folders Jellyfin holds
// the show in.
type TakeOver struct {
	Folders []string `json:"folders"`
	// Files counts the files Garner took over; Episodes the episodes they
	// hold.
	Files    int `json:"files"`
	Episodes int `json:"episodes"`
	// Skipped are the video files Garner left alone, and why.
	Skipped []SkippedFile `json:"skipped"`
	// Renamable counts the show's taken-over files outside Garner's folder
	// for it; renaming moves them there under Garner's names.
	Renamable int `json:"renamable"`
	// Notes are problems that did not stop the search, such as Jellyfin
	// not answering.
	Notes []string `json:"notes"`
}

// SkippedFile is a video file Garner did not take over.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Renamed is what renaming a show's taken-over files did.
type Renamed struct {
	// Files counts the files moved into Garner's folder for the show.
	Files int `json:"files"`
	// Notes are what stayed behind, and why.
	Notes []string `json:"notes"`
}

// ProfileInfo names a profile and its rules for a show. Preferences is
// the rules' list of preferences alone, best first.
type ProfileInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Rules       string `json:"rules"`
	Preferences string `json:"preferences"`
}

// Issue is a problem with imported data that needs a review.
type Issue struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// SearchResult is a show TVDB found.
type SearchResult struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug,omitempty"`
	Name     string `json:"name"`
	NameEng  string `json:"name_eng,omitempty"`
	NameDeu  string `json:"name_deu,omitempty"`
	Year     int    `json:"year,omitempty"`
	Status   string `json:"status,omitempty"`
	Language string `json:"language,omitempty"`
	Network  string `json:"network,omitempty"`
	Added    bool   `json:"added"`
}

// SearchPage is what a search found: up to 50 shows, best first, and how
// many TVDB knows in all.
type SearchPage struct {
	Results []SearchResult `json:"results"`
	Total   int            `json:"total"`
}

// Preview is a show as TVDB describes it, for deciding whether to add it.
type Preview struct {
	SearchResult
	// Type is the profile Garner will give the show: anime for TVDB's
	// Anime genre, otherwise tv. Rules describes that profile's tiers.
	Type  string `json:"type"`
	Rules string `json:"rules"`
	// LanguageName is the original language in English, such as
	// "Japanese"; LanguageNote says what it means for the audio.
	LanguageName string `json:"language_name"`
	LanguageNote string `json:"language_note"`
	FirstAired   string `json:"first_aired,omitempty"`
	LastAired    string `json:"last_aired,omitempty"`
	Runtime      int    `json:"runtime,omitempty"`
	// Orders are TVDB's episode orders for the show, the default first,
	// such as "Aired Order" and "Alternate Order (Netflix)".
	Orders []string `json:"orders"`
	// Overview is in English when TVDB has it, else German, else the
	// original language; OverviewLanguage says which.
	Overview         string `json:"overview,omitempty"`
	OverviewLanguage string `json:"overview_language,omitempty"`
	// Seasons and Episodes count the default order, specials excluded.
	Seasons  int    `json:"seasons"`
	Episodes int    `json:"episodes"`
	URL      string `json:"url"`
}
