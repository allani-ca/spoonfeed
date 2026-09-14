package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fxamacker/cbor/v2"
	_ "modernc.org/sqlite"
)

type Config struct {
	ActorsPath      string
	Listen          string
	Refresh         time.Duration
	FeedURI         string
	MaxAge          time.Duration
	Hostname        string
	ServiceDID      string
	PublisherDID    string
	PublisherHandle string
}

type StrongRef struct {
	CID string `cbor:"cid"`
	URI string `cbor:"uri"`
}

type ReplyRef struct {
	Root   StrongRef `cbor:"root"`
	Parent StrongRef `cbor:"parent"`
}

type PostRecord struct {
	Type      string    `cbor:"$type"`
	Text      string    `cbor:"text"`
	CreatedAt time.Time `cbor:"createdAt"`
	Reply     *ReplyRef `cbor:"reply"`
}

type LikeRecord struct {
	Type    string    `cbor:"$type"`
	Subject StrongRef `cbor:"subject"`
}

type Post struct {
	URI       string
	CID       string
	DID       string
	RKey      string
	Text      string
	CreatedAt time.Time
	Likes     int
	Replies   int
	Score     float64
}

type Index struct {
	Posts map[string]*Post
}

type Server struct {
	cfg   Config
	index atomic.Pointer[Index]
}

type skeletonResponse struct {
	Feed   []skeletonItem `json:"feed"`
	Cursor string         `json:"cursor,omitempty"`
}

type skeletonItem struct {
	Post string `json:"post"`
}

type describeFeedGeneratorResponse struct {
	DID   string              `json:"did"`
	Feeds []describeFeedItem  `json:"feeds"`
	Links *describeFeedLinks `json:"links,omitempty"`
}

type describeFeedItem struct {
	URI string `json:"uri"`
}

type describeFeedLinks struct {
	PrivacyPolicy  string `json:"privacyPolicy,omitempty"`
	TermsOfService string `json:"termsOfService,omitempty"`
}

type DIDDocument struct {
	Context []string     `json:"@context,omitempty"`
	ID      string       `json:"id"`
	Service []DIDService `json:"service"`
}

type DIDService struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ServiceEndpoint string `json:"serviceEndpoint"`
}

func loadEnv(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
			(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
			if len(val) >= 2 {
				val = val[1 : len(val)-1]
			}
		}

		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}

	return nil
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}

func parseConfig() Config {
	_ = loadEnv(".env")

	defaultListen := getEnv("FEEDGEN_LISTEN", ":4040")
	if p := os.Getenv("PORT"); p != "" && os.Getenv("FEEDGEN_LISTEN") == "" {
		if !strings.HasPrefix(p, ":") {
			defaultListen = ":" + p
		} else {
			defaultListen = p
		}
	}

	defaultHostname := getEnv("FEEDGEN_HOSTNAME", "spoonfeed.devsky.app")
	defaultServiceDID := getEnv("FEEDGEN_SERVICE_DID", "did:web:"+defaultHostname)
	defaultActors := getEnv("FEEDGEN_ACTORS_PATH", "/pds/actors")
	defaultRefreshStr := getEnv("FEEDGEN_REFRESH", "30s")
	defaultRefresh, err := time.ParseDuration(defaultRefreshStr)
	if err != nil {
		defaultRefresh = 30 * time.Second
	}

	defaultFeedURI := getEnv("FEEDGEN_FEED_URI", "at://did:plc:nuzd73csefxfqwnuprvomqbp/app.bsky.feed.generator/spoonfeed")
	defaultMaxAgeStr := getEnv("FEEDGEN_MAX_AGE", "720h")
	defaultMaxAge, err := time.ParseDuration(defaultMaxAgeStr)
	if err != nil {
		defaultMaxAge = 30 * 24 * time.Hour
	}

	defaultPublisherDID := getEnv("FEEDGEN_PUBLISHER_DID", "did:plc:nuzd73csefxfqwnuprvomqbp")
	defaultPublisherHandle := getEnv("FEEDGEN_PUBLISHER_HANDLE", "operator.devsky.app")

	var cfg Config
	flag.StringVar(&cfg.ActorsPath, "actors", defaultActors, "PDS actors directory")
	flag.StringVar(&cfg.Listen, "listen", defaultListen, "HTTP listen address")
	flag.DurationVar(&cfg.Refresh, "refresh", defaultRefresh, "full index refresh interval")
	flag.StringVar(&cfg.FeedURI, "feed-uri", defaultFeedURI, "AT URI of this feed generator")
	flag.DurationVar(&cfg.MaxAge, "max-age", defaultMaxAge, "maximum age of posts to index")
	flag.StringVar(&cfg.Hostname, "hostname", defaultHostname, "Public hostname of this feed generator")
	flag.StringVar(&cfg.ServiceDID, "service-did", defaultServiceDID, "Service DID of this feed generator")
	flag.StringVar(&cfg.PublisherDID, "publisher-did", defaultPublisherDID, "DID of feed publisher")
	flag.StringVar(&cfg.PublisherHandle, "publisher-handle", defaultPublisherHandle, "Handle of feed publisher")
	flag.Parse()

	return cfg
}

func main() {
	cfg := parseConfig()

	s := &Server{cfg: cfg}

	if err := s.refresh(); err != nil {
		log.Fatalf("initial index: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.refreshLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/did.json", s.wellKnownDID)
	mux.HandleFunc("/xrpc/app.bsky.feed.getFeedSkeleton", s.getFeedSkeleton)
	mux.HandleFunc("/xrpc/app.bsky.feed.describeFeedGenerator", s.describeFeedGenerator)
	mux.HandleFunc("/healthz", s.health)

	log.Printf("spoonfeed listening on %s", cfg.Listen)
	log.Printf("actors=%s refresh=%s max-age=%s feed=%s hostname=%s did=%s",
		cfg.ActorsPath, cfg.Refresh, cfg.MaxAge, cfg.FeedURI, cfg.Hostname, cfg.ServiceDID)

	if err := http.ListenAndServe(cfg.Listen, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *Server) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Refresh)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.refresh(); err != nil {
				log.Printf("index refresh failed: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) refresh() error {
	started := time.Now()

	idx, err := buildIndex(s.cfg.ActorsPath, s.cfg.MaxAge)
	if err != nil {
		return err
	}

	s.index.Store(idx)

	log.Printf("index rebuilt: %d posts in %s",
		len(idx.Posts), time.Since(started).Round(time.Millisecond))

	return nil
}

func buildIndex(actorsRoot string, maxAge time.Duration) (*Index, error) {
	stores, err := findStores(actorsRoot)
	if err != nil {
		return nil, err
	}

	idx := &Index{
		Posts: make(map[string]*Post),
	}

	// Pass 1: load all posts before looking at engagement.
	for _, store := range stores {
		if err := scanPosts(store, idx, maxAge); err != nil {
			log.Printf("posts %s: %v", store, err)
		}
	}

	// Pass 2: scan every repository for likes and replies.
	for _, store := range stores {
		if err := scanLikesAndReplies(store, idx); err != nil {
			log.Printf("engagement %s: %v", store, err)
		}
	}

	now := time.Now()
	for _, post := range idx.Posts {
		post.Score = rankPost(post, now)
	}

	return idx, nil
}

func findStores(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var stores []string

	for _, bucket := range entries {
		if !bucket.IsDir() {
			continue
		}

		bucketPath := filepath.Join(root, bucket.Name())
		dids, err := os.ReadDir(bucketPath)
		if err != nil {
			return nil, err
		}

		for _, did := range dids {
			if !did.IsDir() || !strings.HasPrefix(did.Name(), "did:") {
				continue
			}

			store := filepath.Join(bucketPath, did.Name(), "store.sqlite")
			info, err := os.Stat(store)
			if err == nil && !info.IsDir() {
				stores = append(stores, store)
			}
		}
	}

	sort.Strings(stores)
	return stores, nil
}

func openStore(path string) (*sql.DB, error) {
	// Read-only: spoonfeed must never modify PDS storage.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func scanPosts(path string, idx *Index, maxAge time.Duration) error {
	db, err := openStore(path)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT r.uri, r.cid, r.rkey, rb.content
		FROM record r
		JOIN repo_block rb ON rb.cid = r.cid
		WHERE r.collection = 'app.bsky.feed.post'
		  AND r.takedownRef IS NULL
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	cutoff := time.Now().Add(-maxAge)

	for rows.Next() {
		var uri, cid, rkey string
		var content []byte

		if err := rows.Scan(&uri, &cid, &rkey, &content); err != nil {
			return err
		}

		var record PostRecord
		if err := cbor.Unmarshal(content, &record); err != nil {
			log.Printf("decode post %s: %v", uri, err)
			continue
		}

		if record.Type != "" && record.Type != "app.bsky.feed.post" {
			continue
		}

		if record.CreatedAt.Before(cutoff) {
			continue
		}

		did, err := didFromURI(uri)
		if err != nil {
			log.Printf("post %s: %v", uri, err)
			continue
		}

		idx.Posts[uri] = &Post{
			URI:       uri,
			CID:       cid,
			DID:       did,
			RKey:      rkey,
			Text:      record.Text,
			CreatedAt: record.CreatedAt,
		}
	}

	return rows.Err()
}

func scanLikesAndReplies(path string, idx *Index) error {
	db, err := openStore(path)
	if err != nil {
		return err
	}
	defer db.Close()

	// Likes reference another post by AT URI.
	likeRows, err := db.Query(`
		SELECT rb.content
		FROM record r
		JOIN repo_block rb ON rb.cid = r.cid
		WHERE r.collection = 'app.bsky.feed.like'
		  AND r.takedownRef IS NULL
	`)
	if err != nil {
		return err
	}

	for likeRows.Next() {
		var content []byte
		if err := likeRows.Scan(&content); err != nil {
			likeRows.Close()
			return err
		}

		var record LikeRecord
		if err := cbor.Unmarshal(content, &record); err != nil {
			log.Printf("decode like: %v", err)
			continue
		}

		if post, ok := idx.Posts[record.Subject.URI]; ok {
			post.Likes++
		}
	}

	if err := likeRows.Err(); err != nil {
		likeRows.Close()
		return err
	}
	likeRows.Close()

	// Replies are posts whose "reply.parent.uri" points at another post.
	replyRows, err := db.Query(`
		SELECT rb.content
		FROM record r
		JOIN repo_block rb ON rb.cid = r.cid
		WHERE r.collection = 'app.bsky.feed.post'
		  AND r.takedownRef IS NULL
	`)
	if err != nil {
		return err
	}

	for replyRows.Next() {
		var content []byte
		if err := replyRows.Scan(&content); err != nil {
			replyRows.Close()
			return err
		}

		var record PostRecord
		if err := cbor.Unmarshal(content, &record); err != nil {
			log.Printf("decode reply/post: %v", err)
			continue
		}

		if record.Reply == nil || record.Reply.Parent.URI == "" {
			continue
		}

		if post, ok := idx.Posts[record.Reply.Parent.URI]; ok {
			post.Replies++
		}
	}

	if err := replyRows.Err(); err != nil {
		replyRows.Close()
		return err
	}
	replyRows.Close()

	return nil
}

func didFromURI(uri string) (string, error) {
	parts := strings.Split(uri, "/")
	if len(parts) < 5 || parts[0] != "at:" || parts[2] == "" {
		return "", fmt.Errorf("invalid AT URI %q", uri)
	}
	return parts[2], nil
}

func rankPost(post *Post, now time.Time) float64 {
	ageHours := now.Sub(post.CreatedAt).Hours()
	if ageHours < 0 {
		ageHours = 0
	}

	// Replies are currently worth twice a like.
	points := float64(post.Likes + post.Replies*2)

	// HN-ish gravity:
	//   max(points-1, 0) / (ageHours+2)^1.8
	return math.Max(points-1, 0) / math.Pow(ageHours+2, 1.8)
}

func (s *Server) getFeedSkeleton(w http.ResponseWriter, r *http.Request) {
	idx := s.index.Load()
	if idx == nil {
		http.Error(w, "index unavailable", http.StatusServiceUnavailable)
		return
	}

	if feed := r.URL.Query().Get("feed"); feed != "" && feed != s.cfg.FeedURI {
		http.Error(w, "unknown feed", http.StatusBadRequest)
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}

	offset, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "invalid cursor", http.StatusBadRequest)
		return
	}

	posts := make([]*Post, 0, len(idx.Posts))
	for _, post := range idx.Posts {
		posts = append(posts, post)
	}

	sort.Slice(posts, func(i, j int) bool {
		if posts[i].Score == posts[j].Score {
			if posts[i].CreatedAt.Equal(posts[j].CreatedAt) {
				return posts[i].URI < posts[j].URI
			}
			return posts[i].CreatedAt.After(posts[j].CreatedAt)
		}
		return posts[i].Score > posts[j].Score
	})

	if offset > len(posts) {
		offset = len(posts)
	}

	end := offset + limit
	if end > len(posts) {
		end = len(posts)
	}

	response := skeletonResponse{
		Feed: make([]skeletonItem, 0, end-offset),
	}

	for _, post := range posts[offset:end] {
		response.Feed = append(response.Feed, skeletonItem{
			Post: post.URI,
		})
	}

	if end < len(posts) {
		response.Cursor = encodeCursor(end)
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) wellKnownDID(w http.ResponseWriter, r *http.Request) {
	serviceEndpoint := "https://" + s.cfg.Hostname
	if strings.HasPrefix(s.cfg.Hostname, "http://") || strings.HasPrefix(s.cfg.Hostname, "https://") {
		serviceEndpoint = s.cfg.Hostname
	}

	doc := DIDDocument{
		ID: s.cfg.ServiceDID,
		Service: []DIDService{
			{
				ID:              "#bsky_fg",
				Type:            "BskyFeedGenerator",
				ServiceEndpoint: serviceEndpoint,
			},
		},
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) describeFeedGenerator(w http.ResponseWriter, r *http.Request) {
	feeds := make([]describeFeedItem, 0, 1)
	if s.cfg.FeedURI != "" {
		feeds = append(feeds, describeFeedItem{
			URI: s.cfg.FeedURI,
		})
	}

	response := describeFeedGeneratorResponse{
		DID:   s.cfg.ServiceDID,
		Feeds: feeds,
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	idx := s.index.Load()

	count := 0
	if idx != nil {
		count = len(idx.Posts)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    idx != nil,
		"posts": count,
	})
}

func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}

	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}

	n, err := strconv.Atoi(string(data))
	if err != nil || n < 0 {
		return 0, errors.New("bad cursor")
	}

	return n, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
