package db

import (
	"time"
)

//Podcast is
type Podcast struct {
	Base
	Title string

	Summary string `gorm:"type:text"`

	Author string

	Image string

	URL string

	LastEpisode *time.Time

	PodcastItems []PodcastItem

	Tags []*Tag `gorm:"many2many:podcast_tags;"`

	DownloadedEpisodesCount  int `gorm:"-"`
	DownloadingEpisodesCount int `gorm:"-"`
	AllEpisodesCount         int `gorm:"-"`

	DownloadedEpisodesSize  int64 `gorm:"-"`
	DownloadingEpisodesSize int64 `gorm:"-"`
	AllEpisodesSize         int64 `gorm:"-"`

	IsPaused bool `gorm:"default:false"`
}

//PodcastItem is
type PodcastItem struct {
	Base
	PodcastID string `gorm:"index"`
	Podcast   Podcast
	Title     string
	Summary   string `gorm:"type:text"`

	EpisodeType string

	Duration int

	PubDate time.Time

	FileURL string

	GUID  string `gorm:"index"`
	Image string

	DownloadDate   time.Time
	DownloadPath   string
	DownloadStatus DownloadStatus `gorm:"default:0;index"`

	IsPlayed bool `gorm:"default:false"`

	BookmarkDate time.Time

	LocalImage string

	FileSize int64
	// SHA-256 of the downloaded file's contents. Populated lazily (see
	// BackfillFileHashes) - used to detect when a file being ingested from
	// disk is actually a duplicate of an episode Podgrab already has.
	FileHash string `gorm:"index"`
}

type DownloadStatus int

const (
	NotDownloaded DownloadStatus = iota
	Downloading
	Downloaded
	Deleted
)

type Setting struct {
	Base
	DownloadOnAdd                 bool `gorm:"default:true"`
	InitialDownloadCount          int  `gorm:"default:5"`
	AutoDownload                  bool `gorm:"default:true"`
	AppendDateToFileName          bool `gorm:"default:false"`
	AppendEpisodeNumberToFileName bool `gorm:"default:false"`
	DarkMode                      bool `gorm:"default:false"`
	DownloadEpisodeImages         bool `gorm:"default:false"`
	GenerateNFOFile               bool `gorm:"default:false"`
	DontDownloadDeletedFromDisk   bool `gorm:"default:false"`
	BaseUrl                       string
	MaxDownloadConcurrency        int `gorm:"default:5"`
	UserAgent                     string

	// Cached results from the last filesystem disk-usage scan (see ScanDiskUsage).
	// Populated by a periodic background job and by the manual "Rescan" trigger,
	// not by the user - deliberately left out of the settings edit form/UpdateSettings.
	LastDiskScanTime        time.Time
	DiskScanTotalBytes      int64
	DiskScanKnownBytes      int64
	DiskScanOrphanBytes     int64
	DiskScanOrphanFileCount int
}
type Migration struct {
	Base
	Date time.Time
	Name string
}

type JobLock struct {
	Base
	Date     time.Time
	Name     string
	Duration int
}

type Tag struct {
	Base
	Label       string
	Description string     `gorm:"type:text"`
	Podcasts    []*Podcast `gorm:"many2many:podcast_tags;"`
}

func (lock *JobLock) IsLocked() bool {
	return lock != nil && lock.Date != time.Time{}
}

type PodcastItemStatsModel struct {
	PodcastID      string
	DownloadStatus DownloadStatus
	Count          int
	Size           int64
}

type PodcastItemDiskStatsModel struct {
	DownloadStatus DownloadStatus
	Count          int
	Size           int64
}

type PodcastItemConsolidateDiskStatsModel struct {
	Downloaded           int64
	Downloading          int64
	NotDownloaded        int64
	Deleted              int64
	PendingDownloadCount int64
}

type OrphanFileStatus int

const (
	// OrphanUnmatched: file's folder didn't correspond to any known podcast.
	// Needs manual assignment (Phase 3) - Podgrab won't guess a subscription
	// into existence from a folder name alone.
	OrphanUnmatched OrphanFileStatus = iota
	// OrphanAutoLinked: matched an existing PodcastItem (by normalized title)
	// that had no file yet - linked directly, no new record created.
	OrphanAutoLinked
	// OrphanAutoCreated: podcast was known but no existing episode matched,
	// so a new PodcastItem was created from the file's own metadata.
	OrphanAutoCreated
	// OrphanDuplicate: content hash matched an episode Podgrab already has
	// (either a pre-existing download, or another file ingested in this same
	// run). Never auto-deleted - flagged for the user to review and remove
	// manually if they choose to.
	OrphanDuplicate
	// OrphanIgnored: user has reviewed this and asked Podgrab to leave it alone.
	OrphanIgnored
	// OrphanNonAudio: a real, non-junk file (cover art, .nfo, etc.) that
	// Podgrab recognized as something other than an episode. Recorded for
	// visibility and disk-usage accounting, but never matched or turned
	// into an episode record.
	OrphanNonAudio
)

// OrphanFile records what the library-ingest scan found and decided for a
// single file on disk that wasn't already tracked by Podgrab. This is what
// makes ingestion idempotent - a file already recorded here (in any status)
// is skipped on subsequent scans, so re-running the scan never re-litigates
// a decision or duplicates work.
type OrphanFile struct {
	Base
	FilePath string `gorm:"uniqueIndex"`
	FileSize int64
	FileHash string `gorm:"index"`

	// Best-guess title Podgrab extracted for this file (ID3 tag, else a
	// cleaned-up filename) - shown to the user, and used as the title for
	// any auto-created episode.
	DetectedTitle string

	Status OrphanFileStatus `gorm:"default:0;index"`

	// Podcast this file's folder matched, if any (empty for OrphanUnmatched).
	PodcastID string `gorm:"index"`
	// The PodcastItem this file ended up linked to or duplicating, if any -
	// covers OrphanAutoLinked, OrphanAutoCreated, and OrphanDuplicate.
	PodcastItemID string `gorm:"index"`

	Notes string
}
