package store

import "github.com/trucke/garner/internal/api"

// The store keeps shows, episodes, downloads and searches in the shapes
// the server sends them in, which package api defines.
type (
	Series        = api.Series
	Order         = api.Order
	Title         = api.Title
	Position      = api.Position
	Episode       = api.Episode
	SceneNumber   = api.SceneNumber
	SeriesData    = api.SeriesData
	Show          = api.Show
	Focus         = api.Focus
	EpisodeStatus = api.EpisodeStatus
	Download      = api.Download
	Stage         = api.Stage
	ActivityItem  = api.ActivityItem
	Import        = api.Import
	ImportFile    = api.ImportFile
	Search        = api.Search
	SearchIndexer = api.SearchIndexer
	Candidate     = api.Candidate
)

// Episode, download, import and monitor states.
const (
	MonitorNew         = api.MonitorNew
	MonitorAll         = api.MonitorAll
	MonitorOff         = api.MonitorOff
	EpisodeFailed      = api.EpisodeFailed
	EpisodeAttention   = api.EpisodeAttention
	EpisodeDownloading = api.EpisodeDownloading
	EpisodeProcessing  = api.EpisodeProcessing
	EpisodeImporting   = api.EpisodeImporting
	EpisodeWaiting     = api.EpisodeWaiting
	EpisodeNotSearched = api.EpisodeNotSearched
	EpisodeUpcoming    = api.EpisodeUpcoming
	EpisodeImported    = api.EpisodeImported
	Submitting         = api.Submitting
	Queued             = api.Queued
	Downloading        = api.Downloading
	Processing         = api.Processing
	Completed          = api.Completed
	Failed             = api.Failed
	Cancelled          = api.Cancelled
	ImportPending      = api.ImportPending
	ImportMoving       = api.ImportMoving
	ImportScanning     = api.ImportScanning
	ImportDone         = api.ImportDone
	ImportUnverified   = api.ImportUnverified
	ImportRejected     = api.ImportRejected
	ImportFailed       = api.ImportFailed
)

// MonitorModes lists the monitor modes, the default first.
var MonitorModes = api.MonitorModes
