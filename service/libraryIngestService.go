package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/akhilrex/podgrab/db"
	"github.com/dhowden/tag"
	uuid "github.com/satori/go.uuid"
)

var nonAlphaNumeric = regexp.MustCompile(`[^a-z0-9]+`)

// audioExtensions are the file types IngestLibrary will consider as
// potential episodes. Anything else found on disk (cover art, .nfo files,
// playlists, etc.) is real data and gets accounted for, but is never run
// through title/hash matching or turned into an episode record.
var audioExtensions = map[string]bool{
	".mp3": true, ".m4a": true, ".m4b": true, ".aac": true,
	".ogg": true, ".oga": true, ".opus": true,
	".wav": true, ".flac": true, ".wma": true,
}

// junkFileNames and junkPrefixes are OS/filesystem metadata that carries no
// information worth tracking at all - not even as a non-audio companion
// file. These are skipped entirely, with no OrphanFile record created.
var junkFileNames = map[string]bool{
	".DS_Store": true, "Thumbs.db": true, "desktop.ini": true, ".localized": true,
}

func isJunkFile(fileName string) bool {
	if junkFileNames[fileName] {
		return true
	}
	// AppleDouble metadata files (e.g. "._episode.mp3"), and any other
	// dotfile - legitimate podcast audio is never distributed as a hidden file.
	return strings.HasPrefix(fileName, ".")
}

func isAudioFile(fileName string) bool {
	return audioExtensions[strings.ToLower(filepath.Ext(fileName))]
}

// normalizeForMatch reduces a string to just lowercase letters and digits, so
// "The Best of Car Talk", "the-best-of-car-talk", and "The Best Of Car Talk!"
// all compare equal. Used for both folder-name-to-podcast and
// title-to-episode matching - deliberately aggressive rather than fuzzy, so a
// match is either confidently right or not made at all.
func normalizeForMatch(s string) string {
	return nonAlphaNumeric.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
}

// computeFileHash returns the SHA-256 of a file's contents, used to detect
// when two files are byte-identical regardless of filename.
func computeFileHash(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// detectedTitleFor reads an ID3 (or other supported) tag title from a file if
// present, falling back to a cleaned-up version of the filename when there's
// no tag data - which is expected for some of the older, pre-Podgrab files.
func detectedTitleFor(filePath string) string {
	f, err := os.Open(filePath)
	if err == nil {
		defer f.Close()
		if metadata, tagErr := tag.ReadFrom(f); tagErr == nil {
			if title := strings.TrimSpace(metadata.Title()); title != "" {
				return title
			}
		}
	}

	base := filepath.Base(filePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.Join(strings.Fields(base), " ")
	return base
}

// podcastFolderName returns the first path segment under the assets root for
// a given file - regardless of how deeply the file is actually nested (e.g.
// assets/The Daily/Archive/2019-03-04.mp3 -> "The Daily"). This is what lets
// an arbitrary subfolder structure (like an "Archive" folder per podcast)
// still resolve correctly to the right show.
func podcastFolderName(dataPath string, filePath string) string {
	rel, err := filepath.Rel(dataPath, filePath)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// podcastHashCache and podcastTitleCache let a single ingest run avoid
// re-querying the database for every file - loaded lazily per podcast the
// first time a file in that podcast's folder is encountered, then kept
// up to date in memory as new episodes get linked/created during the run.
type ingestCaches struct {
	podcastByFolder map[string]db.Podcast
	hashesByPodcast map[string]map[string]string          // podcastID -> hash -> podcastItemID
	titlesByPodcast map[string]map[string]*db.PodcastItem // podcastID -> normalized title -> item
}

func newIngestCaches() (*ingestCaches, error) {
	var podcasts []db.Podcast
	if err := db.GetAllPodcasts(&podcasts, ""); err != nil {
		return nil, err
	}
	byFolder := make(map[string]db.Podcast, len(podcasts))
	for _, p := range podcasts {
		byFolder[normalizeForMatch(p.Title)] = p
	}
	return &ingestCaches{
		podcastByFolder: byFolder,
		hashesByPodcast: make(map[string]map[string]string),
		titlesByPodcast: make(map[string]map[string]*db.PodcastItem),
	}, nil
}

func (c *ingestCaches) hashesFor(podcastID string) (map[string]string, error) {
	if existing, ok := c.hashesByPodcast[podcastID]; ok {
		return existing, nil
	}
	hashes, err := db.GetPodcastItemHashesByPodcastId(podcastID)
	if err != nil {
		return nil, err
	}
	c.hashesByPodcast[podcastID] = hashes
	return hashes, nil
}

func (c *ingestCaches) titlesFor(podcastID string) (map[string]*db.PodcastItem, error) {
	if existing, ok := c.titlesByPodcast[podcastID]; ok {
		return existing, nil
	}
	var items []db.PodcastItem
	if err := db.GetAllPodcastItemsByPodcastId(podcastID, &items); err != nil {
		return nil, err
	}
	titles := make(map[string]*db.PodcastItem, len(items))
	for i := range items {
		titles[normalizeForMatch(items[i].Title)] = &items[i]
	}
	c.titlesByPodcast[podcastID] = titles
	return titles, nil
}

// IngestLibraryResult summarizes what a single ingest run decided.
type IngestLibraryResult struct {
	Unmatched   int
	AutoLinked  int
	AutoCreated int
	Duplicate   int
	NonAudio    int
	Errors      int
}

// IngestLibrary walks the assets directory looking for files Podgrab doesn't
// already know about (via the same known-download-path check used by
// ScanDiskUsage) and, for each one:
//
//  1. Matches its top-level folder to a known podcast. No match -> flagged
//     OrphanUnmatched for manual review; Podgrab never invents a podcast
//     subscription from a folder name alone.
//  2. Within a matched podcast, checks the file's content hash against every
//     episode Podgrab already has downloaded. A hash match -> OrphanDuplicate,
//     nothing new is created, nothing is deleted.
//  3. Otherwise tries to match the file's title (ID3 tag, else filename) to
//     an existing-but-not-yet-downloaded episode record. A match -> the
//     existing record is linked directly (OrphanAutoLinked). A title match
//     against an episode that's already downloaded elsewhere is also treated
//     as a duplicate, since hashing alone won't catch a re-encoded copy.
//  4. Otherwise, a brand new episode record is created from the file's own
//     metadata (OrphanAutoCreated) - this is the common case for archives of
//     episodes that have since rolled off the podcast's live RSS feed.
//
// Every file gets exactly one OrphanFile record, which also makes re-running
// this idempotent: a file already recorded (in any status) is skipped.
func IngestLibrary() IngestLibraryResult {
	var result IngestLibraryResult

	dataPath := os.Getenv("DATA")
	if dataPath == "" {
		fmt.Println("Error running library ingest: DATA path is not configured")
		return result
	}

	knownPaths, err := db.GetAllKnownDownloadPaths()
	if err != nil {
		fmt.Println("Error running library ingest: ", err.Error())
		return result
	}
	knownSet := make(map[string]bool, len(knownPaths))
	for _, p := range knownPaths {
		if p != "" {
			knownSet[filepath.Clean(p)] = true
		}
	}

	caches, err := newIngestCaches()
	if err != nil {
		fmt.Println("Error running library ingest: ", err.Error())
		return result
	}

	walkErr := filepath.Walk(dataPath, func(walkPath string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		cleanPath := filepath.Clean(walkPath)
		if knownSet[cleanPath] {
			return nil // already tracked, not an orphan
		}

		existing, err := db.GetOrphanFileByPath(cleanPath)
		if err != nil {
			result.Errors++
			return nil
		}
		if existing != nil {
			return nil // already processed in a previous run
		}

		fileName := info.Name()
		if isJunkFile(fileName) {
			return nil // OS/filesystem metadata - not worth even recording
		}
		if !isAudioFile(fileName) {
			recordNonAudioFile(dataPath, cleanPath, info.Size(), &result)
			return nil
		}

		ingestOneFile(dataPath, cleanPath, info.Size(), caches, &result)
		return nil
	})
	if walkErr != nil {
		fmt.Println("Error walking assets directory during library ingest: ", walkErr.Error())
	}

	return result
}

// recordNonAudioFile tracks a real, non-junk, non-audio file (cover art,
// .nfo, etc.) for visibility and disk-usage accounting. It never runs
// hash/title matching and never creates or links an episode - there's no
// such thing as a confident "match" for a jpg.
func recordNonAudioFile(dataPath string, filePath string, fileSize int64, result *IngestLibraryResult) {
	folderName := podcastFolderName(dataPath, filePath)
	orphan := &db.OrphanFile{
		FilePath:      filePath,
		FileSize:      fileSize,
		DetectedTitle: filepath.Base(filePath),
		Status:        db.OrphanNonAudio,
		Notes:         "Non-audio file found in folder: " + folderName,
	}
	if err := db.CreateOrphanFile(orphan); err != nil {
		result.Errors++
		return
	}
	result.NonAudio++
}

func ingestOneFile(dataPath string, filePath string, fileSize int64, caches *ingestCaches, result *IngestLibraryResult) {
	folderName := podcastFolderName(dataPath, filePath)
	podcast, podcastKnown := caches.podcastByFolder[normalizeForMatch(folderName)]

	title := detectedTitleFor(filePath)

	if !podcastKnown {
		orphan := &db.OrphanFile{
			FilePath:      filePath,
			FileSize:      fileSize,
			DetectedTitle: title,
			Status:        db.OrphanUnmatched,
		}
		if err := db.CreateOrphanFile(orphan); err != nil {
			result.Errors++
			return
		}
		result.Unmatched++
		return
	}

	hash, err := computeFileHash(filePath)
	if err != nil {
		fmt.Println("Error hashing file during library ingest: ", filePath, err.Error())
		result.Errors++
		return
	}

	hashes, err := caches.hashesFor(podcast.ID)
	if err != nil {
		result.Errors++
		return
	}
	if existingItemID, isDuplicate := hashes[hash]; isDuplicate {
		saveOrphan(filePath, fileSize, hash, title, db.OrphanDuplicate, podcast.ID, existingItemID, result, &result.Duplicate)
		return
	}

	titles, err := caches.titlesFor(podcast.ID)
	if err != nil {
		result.Errors++
		return
	}
	if matchedItem, found := titles[normalizeForMatch(title)]; found {
		if matchedItem.DownloadPath == "" {
			// Existing episode record, no file yet - link this file to it directly.
			matchedItem.DownloadPath = filePath
			matchedItem.FileSize = fileSize
			matchedItem.FileHash = hash
			matchedItem.DownloadStatus = db.Downloaded
			matchedItem.DownloadDate = time.Now()
			if err := db.UpdatePodcastItem(matchedItem); err != nil {
				result.Errors++
				return
			}
			hashes[hash] = matchedItem.ID
			saveOrphan(filePath, fileSize, hash, title, db.OrphanAutoLinked, podcast.ID, matchedItem.ID, result, &result.AutoLinked)
			return
		}
		// Title matches an episode that's already downloaded elsewhere -
		// almost certainly the same episode, re-encoded or re-named. Treat
		// as a duplicate rather than creating a second record for it.
		saveOrphan(filePath, fileSize, hash, title, db.OrphanDuplicate, podcast.ID, matchedItem.ID, result, &result.Duplicate)
		return
	}

	// No existing record matches - this is a genuinely new episode as far as
	// Podgrab is concerned (the common case for episodes that have since
	// rolled off the live RSS feed). Create one from the file's own metadata.
	pubDate := time.Now()
	if stat, statErr := os.Stat(filePath); statErr == nil {
		pubDate = stat.ModTime()
	}
	newItem := &db.PodcastItem{
		PodcastID:      podcast.ID,
		Title:          title,
		GUID:           "podgrab-ingested-" + uuid.NewV4().String(),
		PubDate:        pubDate,
		DownloadDate:   time.Now(),
		DownloadPath:   filePath,
		DownloadStatus: db.Downloaded,
		FileSize:       fileSize,
		FileHash:       hash,
	}
	if err := db.CreatePodcastItem(newItem); err != nil {
		result.Errors++
		return
	}
	hashes[hash] = newItem.ID
	titles[normalizeForMatch(title)] = newItem
	saveOrphan(filePath, fileSize, hash, title, db.OrphanAutoCreated, podcast.ID, newItem.ID, result, &result.AutoCreated)
}

func saveOrphan(filePath string, fileSize int64, hash string, title string, status db.OrphanFileStatus, podcastID string, podcastItemID string, result *IngestLibraryResult, counter *int) {
	orphan := &db.OrphanFile{
		FilePath:      filePath,
		FileSize:      fileSize,
		FileHash:      hash,
		DetectedTitle: title,
		Status:        status,
		PodcastID:     podcastID,
		PodcastItemID: podcastItemID,
	}
	if err := db.CreateOrphanFile(orphan); err != nil {
		result.Errors++
		return
	}
	*counter++
}

// BackfillFileHashes computes FileHash for previously-downloaded episodes
// that don't have one yet (i.e. everything downloaded before this feature
// existed), a small batch at a time so it doesn't block other work. Without
// this, IngestLibrary could never detect a disk file as a duplicate of an
// episode Podgrab downloaded normally through its own RSS-based flow.
func BackfillFileHashes() {
	items, err := db.GetPodcastItemsMissingHash(200)
	if err != nil {
		fmt.Println("Error fetching episodes for hash backfill: ", err.Error())
		return
	}
	for _, item := range *items {
		if item.DownloadPath == "" {
			continue
		}
		hash, err := computeFileHash(item.DownloadPath)
		if err != nil {
			// File likely moved/deleted since the DB record was written -
			// skip it rather than fail the whole batch.
			continue
		}
		item.FileHash = hash
		db.UpdatePodcastItem(&item)
	}
}
