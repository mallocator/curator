package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WatchedPoll is the safety net for missed webhooks: it asks Jellyfin (every user) and Plex (the owner's account)
// which episodes are marked watched, and queues those marked since WATCHED_LOOKBACK that it hasn't seen yet.
// A server's watched mark is the truth, so an episode marked watched by hand counts too, and a viewing on one server
// counts without waiting for WatchState to sync it to the other.
//
// Safeguards: the first run only records what is already marked (baseline, nothing queued); a mark that is already
// older than WATCHED_MAX_AGE when it first shows up is a synced one (WatchState copies the original date, often hours
// or days later, and has copied wrong marks) and is ignored; and a poll finding more than WATCHED_BURST_MAX new marks
// (a mass sync or a "mark season watched") queues none of them, so a faulty sync can't trigger mass deletion.
// MaxAge must cover a long playback, because Jellyfin dates a viewing from its start. Deletion rules are unchanged:
// Sonarr tag, then the grace period. With MOVIE_DELETE_USERS movies are polled too (movie_cleanup.go).
func (a *App) WatchedPoll() {
	since := a.now().Add(-a.Config.WatchedLookback)
	movies := len(a.Config.MovieDeleteUsers) > 0
	var found []polledWatch
	if a.Jellyfin != nil {
		w, err := a.Jellyfin.PlayedEpisodes(since)
		if err != nil {
			log.Printf("Watched poll: %v", err)
		}
		found = append(found, w...)
		if movies {
			w, err := a.Jellyfin.PlayedMoviesSince(since)
			if err != nil {
				log.Printf("Watched poll: %v", err)
			}
			found = append(found, w...)
		}
	}
	if a.Plex != nil {
		w, err := a.Plex.WatchedEpisodes(since)
		if err != nil {
			log.Printf("Watched poll: %v", err)
		}
		found = append(found, w...)
		if movies {
			w, err := a.Plex.WatchedMovies(since)
			if err != nil {
				log.Printf("Watched poll: %v", err)
			}
			found = append(found, w...)
		}
	}
	seen := a.seenWatched()
	baseline := seen.Empty()
	var fresh, stale []polledWatch
	for _, w := range found {
		if seen.Has(w.key()) {
			continue
		}
		seen.Add(w.key(), w.At)
		if !baseline && w.At.Before(a.now().Add(-a.Config.WatchedMaxAge)) {
			stale = append(stale, w)
		} else {
			fresh = append(fresh, w)
		}
	}
	for _, w := range stale {
		log.Printf("Watched poll: ignoring %s: marked %s ago, probably synced from the other server", w, a.now().Sub(w.At).Round(time.Minute))
	}
	metrics.Add("curator_watched_poll_stale_total", float64(len(stale)))
	seen.Prune(since.Add(-24 * time.Hour))
	if err := seen.Save(); err != nil {
		log.Printf("Watched poll: saving %s: %v", seen.path, err)
	}
	switch {
	case baseline:
		log.Printf("Watched poll: first run, recorded %d watched marks since %s as known (nothing queued)", len(fresh), since.Format(time.RFC3339))
		return
	case len(fresh) > a.Config.WatchedBurstMax:
		log.Printf("Watched poll: %d new watched marks at once (more than %d), probably a sync; not queuing them:", len(fresh), a.Config.WatchedBurstMax)
		for _, w := range fresh {
			log.Printf("  held: %s", w)
		}
		metrics.Add("curator_watched_poll_held_total", float64(len(fresh)))
		return
	}
	for _, w := range fresh {
		if w.Movie {
			a.watchedAt(WatchEvent{Source: w.Source + "-poll", Type: "movie", Title: w.Title, Year: w.Year,
				TmdbID: w.TmdbID, User: w.User}, w.At)
			continue
		}
		a.watchedAt(WatchEvent{Source: w.Source + "-poll", Type: "episode", Series: w.Series, Season: w.Season,
			Episode: w.Episode, Title: w.Title, User: w.User}, w.At)
	}
}

// polledWatch is an episode or movie a server reports as watched.
type polledWatch struct {
	Source  string // "jellyfin" or "plex"
	User    string
	ItemID  string
	Movie   bool
	Series  string
	Season  int
	Episode int
	Title   string
	Year    int       // movies
	TmdbID  int       // movies
	At      time.Time // when it was played or marked watched
}

// key identifies one watched mark; watching the episode again later is a new mark.
func (w polledWatch) key() string {
	return fmt.Sprintf("%s|%s|%s|%d", w.Source, w.User, w.ItemID, w.At.Unix())
}

func (w polledWatch) String() string {
	if w.Movie {
		return fmt.Sprintf("%s (%d) (%s %s, %s)", w.Title, w.Year, w.Source, w.User, w.At.Format("2006-01-02 15:04"))
	}
	return fmt.Sprintf("%s S%02dE%02d (%s %s, %s)", w.Series, w.Season, w.Episode, w.Source, w.User, w.At.Format("2006-01-02 15:04"))
}

// PlayedEpisodes returns the episodes each Jellyfin user marked played since the given time.
func (j *Jellyfin) PlayedEpisodes(since time.Time) ([]polledWatch, error) {
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := j.get("/Users", &users); err != nil {
		return nil, err
	}
	var out []polledWatch
	for _, u := range users {
		for start := 0; ; start += 100 {
			var page struct {
				Items []struct {
					ID         string `json:"Id"`
					Name       string `json:"Name"`
					SeriesName string `json:"SeriesName"`
					Season     int    `json:"ParentIndexNumber"`
					Episode    int    `json:"IndexNumber"`
					UserData   struct {
						LastPlayedDate string `json:"LastPlayedDate"`
					} `json:"UserData"`
				} `json:"Items"`
			}
			q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Episode"}, "IsPlayed": {"true"},
				"SortBy": {"DatePlayed"}, "SortOrder": {"Descending"}, "EnableUserData": {"true"},
				"StartIndex": {strconv.Itoa(start)}, "Limit": {"100"}}
			if err := j.get("/Users/"+u.ID+"/Items?"+q.Encode(), &page); err != nil {
				return out, err
			}
			older := false
			for _, it := range page.Items {
				at, err := time.Parse(time.RFC3339Nano, it.UserData.LastPlayedDate)
				if err != nil || it.SeriesName == "" {
					continue // marked played without a date: can't tell when, so it can't be "new"
				}
				if at.Before(since) {
					older = true
					break
				}
				out = append(out, polledWatch{Source: "jellyfin", User: u.Name, ItemID: it.ID, Series: it.SeriesName,
					Season: it.Season, Episode: it.Episode, Title: it.Name, At: at})
			}
			if older || len(page.Items) < 100 {
				break
			}
		}
	}
	return out, nil
}

// PlayedMoviesSince returns the movies each Jellyfin user marked played since the given time.
func (j *Jellyfin) PlayedMoviesSince(since time.Time) ([]polledWatch, error) {
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := j.get("/Users", &users); err != nil {
		return nil, err
	}
	var out []polledWatch
	for _, u := range users {
		var page struct {
			Items []struct {
				ID          string            `json:"Id"`
				Name        string            `json:"Name"`
				Year        int               `json:"ProductionYear"`
				ProviderIds map[string]string `json:"ProviderIds"`
				UserData    struct {
					LastPlayedDate string `json:"LastPlayedDate"`
				} `json:"UserData"`
			} `json:"Items"`
		}
		q := url.Values{"Recursive": {"true"}, "IncludeItemTypes": {"Movie"}, "IsPlayed": {"true"},
			"SortBy": {"DatePlayed"}, "SortOrder": {"Descending"}, "EnableUserData": {"true"}, "Fields": {"ProviderIds"},
			"Limit": {"100"}}
		if err := j.get("/Users/"+u.ID+"/Items?"+q.Encode(), &page); err != nil {
			return out, err
		}
		for _, it := range page.Items {
			at, err := time.Parse(time.RFC3339Nano, it.UserData.LastPlayedDate)
			if err != nil {
				continue
			}
			if at.Before(since) {
				break
			}
			tmdb := 0
			for k, v := range it.ProviderIds {
				if strings.EqualFold(k, "tmdb") {
					tmdb, _ = strconv.Atoi(v)
				}
			}
			out = append(out, polledWatch{Source: "jellyfin", User: u.Name, ItemID: it.ID, Movie: true, Title: it.Name,
				Year: it.Year, TmdbID: tmdb, At: at})
		}
	}
	return out, nil
}

// WatchedMovies returns the movies the Plex owner account watched since the given time.
func (p *Plex) WatchedMovies(since time.Time) ([]polledWatch, error) {
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.get("/library/sections", &sections); err != nil {
		return nil, err
	}
	var out []polledWatch
	for _, d := range sections.MediaContainer.Directory {
		if d.Type != "movie" {
			continue
		}
		var items struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey  string `json:"ratingKey"`
					Type       string `json:"type"`
					Title      string `json:"title"`
					Year       int    `json:"year"`
					LastViewed int64  `json:"lastViewedAt"`
					Guid       []struct {
						ID string `json:"id"`
					} `json:"Guid"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		path := fmt.Sprintf("/library/sections/%s/all?type=1&includeGuids=1&sort=lastViewedAt:desc&lastViewedAt%%3E%%3E=%d", d.Key, since.Unix())
		if err := p.get(path, &items); err != nil {
			return out, err
		}
		for _, m := range items.MediaContainer.Metadata {
			if m.LastViewed == 0 || m.Type != "movie" {
				continue
			}
			var guids []string
			for _, g := range m.Guid {
				guids = append(guids, g.ID)
			}
			out = append(out, polledWatch{Source: "plex", User: "owner", ItemID: m.RatingKey, Movie: true, Title: m.Title,
				Year: m.Year, TmdbID: plexTmdbID(guids), At: time.Unix(m.LastViewed, 0)})
		}
	}
	return out, nil
}

// WatchedEpisodes returns the episodes the Plex owner account (the tokenless local API) watched since the given time.
// Other Plex accounts are covered by the media.scrobble webhook.
func (p *Plex) WatchedEpisodes(since time.Time) ([]polledWatch, error) {
	var sections struct {
		MediaContainer struct {
			Directory []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.get("/library/sections", &sections); err != nil {
		return nil, err
	}
	var out []polledWatch
	for _, d := range sections.MediaContainer.Directory {
		if d.Type != "show" {
			continue
		}
		var items struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey  string `json:"ratingKey"`
					Title      string `json:"title"`
					Series     string `json:"grandparentTitle"`
					Season     int    `json:"parentIndex"`
					Episode    int    `json:"index"`
					LastViewed int64  `json:"lastViewedAt"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		path := fmt.Sprintf("/library/sections/%s/all?type=4&sort=lastViewedAt:desc&lastViewedAt%%3E%%3E=%d", d.Key, since.Unix())
		if err := p.get(path, &items); err != nil {
			return out, err
		}
		for _, m := range items.MediaContainer.Metadata {
			if m.LastViewed == 0 || m.Series == "" {
				continue
			}
			out = append(out, polledWatch{Source: "plex", User: "owner", ItemID: m.RatingKey, Series: m.Series,
				Season: m.Season, Episode: m.Episode, Title: m.Title, At: time.Unix(m.LastViewed, 0)})
		}
	}
	return out, nil
}

// seenSet remembers the watched marks already handled (key -> unix time of the mark), persisted next to the queue.
type seenSet struct {
	mu     sync.Mutex
	path   string
	marks  map[string]int64
	loaded bool // the file existed: not the first run
}

func (a *App) seenWatched() *seenSet {
	a.seenOnce.Do(func() {
		a.seen = &seenSet{path: a.Config.WatchedSeenFile, marks: map[string]int64{}}
		data, err := os.ReadFile(a.seen.path)
		if err == nil && json.Unmarshal(data, &a.seen.marks) == nil {
			a.seen.loaded = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("Watched poll: reading %s: %v", a.seen.path, err)
		}
	})
	return a.seen
}

func (s *seenSet) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.loaded
}

func (s *seenSet) Has(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.marks[k]
	return ok
}

func (s *seenSet) Add(k string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks[k] = at.Unix()
}

func (s *seenSet) Prune(before time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.marks {
		if t < before.Unix() {
			delete(s.marks, k)
		}
	}
}

func (s *seenSet) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.marks)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.loaded = true
	return nil
}
