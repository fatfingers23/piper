package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync" // Added for mutex
	"time"

	"github.com/teal-fm/piper/db"
	"github.com/teal-fm/piper/models"
	"golang.org/x/time/rate"
)

// MusicBrainz API Types
type MusicBrainzArtistCredit struct {
	Artist struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		SortName string `json:"sort-name,omitempty"`
	} `json:"artist"`
	Joinphrase string `json:"joinphrase,omitempty"`
	Name       string `json:"name"`
}

type MusicBrainzRelease struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status,omitempty"`
	Date           string `json:"date,omitempty"` // YYYY-MM-DD, YYYY-MM, or YYYY
	Country        string `json:"country,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	TrackCount     int    `json:"track-count,omitempty"`
}

type MusicBrainzRecording struct {
	ID           string                    `json:"id"`
	Title        string                    `json:"title"`
	Length       int                       `json:"length,omitempty"` // milliseconds
	ISRCs        []string                  `json:"isrcs,omitempty"`
	ArtistCredit []MusicBrainzArtistCredit `json:"artist-credit,omitempty"`
	Releases     []MusicBrainzRelease      `json:"releases,omitempty"`
}

type MusicBrainzSearchResponse struct {
	Created    time.Time              `json:"created"`
	Count      int                    `json:"count"`
	Offset     int                    `json:"offset"`
	Recordings []MusicBrainzRecording `json:"recordings"`
}

type SearchParams struct {
	Track   string
	Artist  string
	Release string
}

// cacheEntry holds the cached data and its expiration time.
type cacheEntry struct {
	recordings []MusicBrainzRecording
	expiresAt  time.Time
}

type MusicBrainzService struct {
	db          *db.DB
	httpClient  *http.Client
	limiter     *rate.Limiter
	searchCache map[string]cacheEntry // In-memory cache for search results
	cacheMutex  sync.RWMutex          // Mutex to protect the cache
	cacheTTL    time.Duration         // Time-to-live for cache entries
	cleaner     MetadataCleaner       // Cleaner for cleaning up expired cache entries
}

// NewMusicBrainzService creates a new service instance with rate limiting and caching.
func NewMusicBrainzService(db *db.DB) *MusicBrainzService {
	// MusicBrainz allows 1 request per second
	limiter := rate.NewLimiter(rate.Every(time.Second), 1)
	// Set a default cache TTL (e.g., 1 hour)
	defaultCacheTTL := 1 * time.Hour

	return &MusicBrainzService{
		db: db,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter:     limiter,
		searchCache: make(map[string]cacheEntry),  // Initialize the cache map
		cacheTTL:    defaultCacheTTL,              // Set the cache TTL
		cleaner:     *NewMetadataCleaner("Latin"), // Initialize the cleaner
		// cacheMutex is zero-value ready
	}
}

// generateCacheKey creates a unique string key for caching based on search parameters.
func generateCacheKey(params SearchParams) string {
	// Use a structured format to avoid collisions and ensure order doesn't matter implicitly
	// url.QueryEscape handles potential special characters in parameters
	return fmt.Sprintf("track=%s&artist=%s&release=%s",
		url.QueryEscape(params.Track),
		url.QueryEscape(params.Artist),
		url.QueryEscape(params.Release))
}

// SearchMusicBrainz searches the MusicBrainz API for recordings, using an in-memory cache.
func (s *MusicBrainzService) SearchMusicBrainz(ctx context.Context, params SearchParams) ([]MusicBrainzRecording, error) {
	// Validate parameters first
	if params.Track == "" && params.Artist == "" && params.Release == "" {
		return nil, fmt.Errorf("at least one search parameter (Track, Artist, Release) must be provided")
	}

	// clean params
	params.Track, _ = s.cleaner.CleanRecording(params.Track)
	params.Artist, _ = s.cleaner.CleanArtist(params.Artist)

	cacheKey := generateCacheKey(params)
	now := time.Now()

	// --- Check Cache (Read Lock) ---
	s.cacheMutex.RLock()
	entry, found := s.searchCache[cacheKey]
	s.cacheMutex.RUnlock()

	if found && now.Before(entry.expiresAt) {
		log.Printf("Cache hit for MusicBrainz search: key=%s", cacheKey)
		// Return the cached data directly. Consider if a deep copy is needed if callers modify results.
		return entry.recordings, nil
	}
	// --- Cache Miss or Expired ---
	if found {
		log.Printf("Cache expired for MusicBrainz search: key=%s", cacheKey)
	} else {
		log.Printf("Cache miss for MusicBrainz search: key=%s", cacheKey)
	}

	// --- Proceed with API call ---
	queryParts := []string{}
	if params.Track != "" {
		queryParts = append(queryParts, fmt.Sprintf(`recording:"%s"`, params.Track))
	}
	if params.Artist != "" {
		queryParts = append(queryParts, fmt.Sprintf(`artist:"%s"`, params.Artist))
	}
	if params.Release != "" {
		queryParts = append(queryParts, fmt.Sprintf(`release:"%s"`, params.Release))
	}
	query := strings.Join(queryParts, " AND ")
	endpoint := fmt.Sprintf("https://musicbrainz.org/ws/2/recording?query=%s&fmt=json&inc=artists+releases+isrcs", url.QueryEscape(query))

	if err := s.limiter.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context cancelled during rate limiter wait: %w", ctx.Err())
		}
		return nil, fmt.Errorf("rate limiter error: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "piper/0.0.1 ( https://github.com/teal-fm/piper )")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context error during request execution: %w", ctx.Err())
		}
		return nil, fmt.Errorf("failed to execute request to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// TODO: Consider reading body for detailed error message from MusicBrainz
		return nil, fmt.Errorf("MusicBrainz API request to %s returned status %d", endpoint, resp.StatusCode)
	}

	var result MusicBrainzSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response from %s: %w", endpoint, err)
	}

	// cache result for later
	s.cacheMutex.Lock()
	s.searchCache[cacheKey] = cacheEntry{
		recordings: result.Recordings,
		expiresAt:  time.Now().Add(s.cacheTTL),
	}
	s.cacheMutex.Unlock()
	log.Printf("Cached MusicBrainz search result for key=%s, TTL=%s", cacheKey, s.cacheTTL)

	// Return the newly fetched results
	return result.Recordings, nil
}

// GetBestRelease selects the 'best' release from a list based on specific criteria.
func GetBestRelease(releases []MusicBrainzRelease, trackTitle string) *MusicBrainzRelease {
	if len(releases) == 0 {
		return nil
	}
	if len(releases) == 1 {
		// Return a pointer to the single element
		r := releases[0]
		return &r
	}

	// Sort releases: Prefer valid dates first, then sort by date, title, id.
	sort.SliceStable(releases, func(i, j int) bool {
		dateA := releases[i].Date
		dateB := releases[j].Date
		validDateA := len(dateA) >= 4 // Basic check for YYYY format or longer
		validDateB := len(dateB) >= 4

		// Put invalid/empty dates at the end
		if validDateA && !validDateB {
			return true
		}
		if !validDateA && validDateB {
			return false
		}
		// If both valid or both invalid, compare dates lexicographically
		if dateA != dateB {
			return dateA < dateB
		}
		// If dates are same, compare by title
		if releases[i].Title != releases[j].Title {
			return releases[i].Title < releases[j].Title
		}
		// If titles are same, compare by ID
		return releases[i].ID < releases[j].ID
	})

	// 1. Find oldest release where country is 'XW' or 'US' AND title is NOT track title
	for i := range releases {
		release := &releases[i]
		if (release.Country == "XW" || release.Country == "US") && release.Title != trackTitle {
			return release
		}
	}

	// 2. If none, find oldest release where title is NOT track title
	for i := range releases {
		release := &releases[i]
		if release.Title != trackTitle {
			return release
		}
	}

	// 3. If none found, return the oldest release overall (which is the first one after sorting)
	log.Printf("Could not find a suitable release for '%s', picking oldest: '%s' (%s)", trackTitle, releases[0].Title, releases[0].ID)
	r := releases[0]
	return &r
}

func HydrateTrack(mb *MusicBrainzService, track models.Track) (*models.Track, error) {
	ctx := context.Background()
	// array of strings
	artistArray := make([]string, len(track.Artist)) // Assuming Name is string type
	for i, a := range track.Artist {
		artistArray[i] = a.Name
	}

	params := SearchParams{
		Track:   track.Name,
		Artist:  strings.Join(artistArray, ", "),
		Release: track.Album,
	}
	res, err := mb.SearchMusicBrainz(ctx, params)
	if err != nil {
		return nil, err
	}

	if len(res) == 0 {
		return nil, errors.New("no results found")
	}

	firstResult := res[0]
	firstResultAlbum := GetBestRelease(firstResult.Releases, firstResult.Title)

	bestISRC := firstResult.ISRCs[0]

	if len(firstResult.ISRCs) == 0 {
		bestISRC = track.ISRC
	}

	artists := make([]models.Artist, len(firstResult.ArtistCredit))

	for i, a := range firstResult.ArtistCredit {
		artists[i] = models.Artist{
			Name: a.Name,
			ID:   a.Artist.ID,
			MBID: a.Artist.ID,
		}
	}

	resTrack := models.Track{
		HasStamped:     track.HasStamped,
		PlayID:         track.PlayID,
		Name:           track.Name,
		URL:            track.URL,
		ServiceBaseUrl: track.ServiceBaseUrl,
		RecordingMBID:  firstResult.ID,
		Album:          firstResultAlbum.Title,
		ReleaseMBID:    firstResultAlbum.ID,
		ISRC:           bestISRC,
		Timestamp:      track.Timestamp,
		ProgressMs:     track.ProgressMs,
		DurationMs:     int64(firstResult.Length),
		Artist:         artists,
	}

	return &resTrack, nil
}
