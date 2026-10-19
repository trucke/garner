package api

import (
	"encoding/json"
	"time"
)

// Download states.
const (
	Submitting  = "submitting" // recorded, SABnzbd not yet confirmed
	Queued      = "queued"
	Downloading = "downloading"
	Processing  = "processing" // SABnzbd repairs, unpacks or moves the files
	Completed   = "completed"
	Failed      = "failed"
	Cancelled   = "cancelled" // stopped on request; not a failure of the release
)

// Download is a release handed to SABnzbd.
type Download struct {
	ID        int64  `json:"id"`
	EpisodeID int64  `json:"episode_id"`
	Title     string `json:"title"`
	Indexer   string `json:"indexer"`
	// Link holds Prowlarr's API key; it is never sent to clients.
	Link    string `json:"-"`
	Size    int64  `json:"size"`
	Verdict string `json:"verdict"`
	Tier    string `json:"tier,omitempty"`
	// Reasons and Decisive are the release's search result: why it was
	// chosen, the first Decisive of them deciding.
	Reasons  []string `json:"reasons"`
	Decisive int      `json:"decisive"`
	Manual   bool     `json:"manual"`
	// Replaces is the tier of the file in the library the download
	// replaces, such as "German 720p"; empty when the episode had no file
	// when it started, or the file's tier is unknown.
	Replaces string `json:"replaces,omitempty"`
	State    string `json:"state"`
	// Detail is SABnzbd's own status or running step, such as "Repairing".
	Detail   string  `json:"detail,omitempty"`
	JobID    string  `json:"job_id,omitempty"`
	Progress float64 `json:"progress"`
	TimeLeft string  `json:"time_left,omitempty"`
	// Storage is where SABnzbd put the files, as SABnzbd sees its disk.
	Storage string `json:"storage,omitempty"`
	Failure string `json:"failure,omitempty"`
	// Retryable means another release may succeed where this one failed.
	Retryable bool      `json:"retryable,omitempty"`
	Stages    []Stage   `json:"stages"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	// Import is the move of the finished files into the library.
	Import Import `json:"import"`
}

// Stage is one of SABnzbd's post-processing steps and what happened in it.
type Stage struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Active reports whether the download is still running.
func (d Download) Active() bool {
	switch d.State {
	case Submitting, Queued, Downloading, Processing:
		return true
	}
	return false
}

// ActivityItem is an episode's latest download with its show and its
// place in the show's default order; Season and Number are 0 when the order
// does not place it.
type ActivityItem struct {
	Download      Download `json:"download"`
	SeriesID      int64    `json:"series_id"`
	SeriesName    string   `json:"series_name"`
	SeriesNameEng string   `json:"series_name_eng,omitempty"`
	Season        int      `json:"season"`
	Number        int      `json:"number"`
}

// Import states.
const (
	ImportPending    = "pending"    // waiting for the file check
	ImportMoving     = "moving"     // checked; files are being moved
	ImportScanning   = "scanning"   // moved; waiting for Jellyfin to list them
	ImportDone       = "imported"   // Jellyfin identified every file as intended
	ImportUnverified = "unverified" // moved, but Jellyfin did not confirm the episodes
	ImportRejected   = "rejected"   // the file check found the release bad
	ImportFailed     = "failed"     // a path or permission problem; see the recovery step
)

// Import is the move of a finished download into the library.
type Import struct {
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Recovery says what to do when the import failed.
	Recovery string       `json:"recovery,omitempty"`
	Files    []ImportFile `json:"files"`
	// Notes are worth showing, such as subtitle files moved or files left
	// in SABnzbd's folder.
	Notes   []string  `json:"notes"`
	Updated time.Time `json:"updated,omitzero"`
}

// ImportFile is one video file of a download.
type ImportFile struct {
	// Source and Target are as Garner reaches them.
	Source   string  `json:"source"`
	Target   string  `json:"target,omitempty"`
	Episodes []int64 `json:"episodes"`
	Size     int64   `json:"size"`
	// Check is the file check's outcome, a FileCheck.
	Check json.RawMessage `json:"check,omitempty"`
	// Jellyfin is how Jellyfin identified the file.
	Jellyfin string `json:"jellyfin,omitempty"`
	Verified bool   `json:"verified,omitempty"`
}

// FileCheck is the outcome of comparing a file with its release.
type FileCheck struct {
	// Problems mean the release is not what it claimed; it is bad.
	Problems []string `json:"problems,omitempty"`
	// Notes are evidence worth showing.
	Notes      []string `json:"notes,omitempty"`
	Resolution string   `json:"resolution,omitempty"`
	Audio      []string `json:"audio"`
	Subtitles  []string `json:"subtitles"`
	Duration   string   `json:"duration,omitempty"`
}

// ResolutionClass names a video's resolution as releases do, such as 1080
// for 1920x800: wide films are cropped in height.
func ResolutionClass(w, h int) int {
	switch {
	case w >= 3200 || h >= 1800:
		return 2160
	case w >= 1800 || h >= 1000:
		return 1080
	case w >= 1200 || h >= 700:
		return 720
	}
	return h
}

// EpisodeDetail is what Garner has of one episode: TVDB's description, its
// file in the library, if any, and every download, newest first.
type EpisodeDetail struct {
	Overview         string       `json:"overview,omitempty"`
	OverviewLanguage string       `json:"overview_language,omitempty"`
	File             *EpisodeFile `json:"file"`
	Downloads        []Download   `json:"downloads"`
}

// EpisodeFile is an episode's file, as Garner reaches it.
type EpisodeFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}
