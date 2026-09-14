package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDIDFromURI(t *testing.T) {
	tests := []struct {
		uri     string
		wantDID string
		wantErr bool
	}{
		{
			uri:     "at://did:plc:123456789/app.bsky.feed.post/3jx123",
			wantDID: "did:plc:123456789",
			wantErr: false,
		},
		{
			uri:     "at://did:web:example.com/app.bsky.feed.post/abc",
			wantDID: "did:web:example.com",
			wantErr: false,
		},
		{
			uri:     "invalid-uri",
			wantErr: true,
		},
		{
			uri:     "at://",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		did, err := didFromURI(tt.uri)
		if (err != nil) != tt.wantErr {
			t.Errorf("didFromURI(%q) error = %v, wantErr %v", tt.uri, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && did != tt.wantDID {
			t.Errorf("didFromURI(%q) = %v, want %v", tt.uri, did, tt.wantDID)
		}
	}
}

func TestCursorEncoding(t *testing.T) {
	offsets := []int{0, 1, 10, 50, 1000}
	for _, off := range offsets {
		encoded := encodeCursor(off)
		decoded, err := decodeCursor(encoded)
		if err != nil {
			t.Fatalf("decodeCursor(%q) failed: %v", encoded, err)
		}
		if decoded != off {
			t.Fatalf("expected offset %d, got %d", off, decoded)
		}
	}

	empty, err := decodeCursor("")
	if err != nil || empty != 0 {
		t.Fatalf("expected 0 for empty cursor, got %d, err %v", empty, err)
	}

	if _, err := decodeCursor("invalid-base64!!!"); err == nil {
		t.Fatalf("expected error on invalid cursor")
	}
}

func TestRankPost(t *testing.T) {
	now := time.Now()
	freshPost := &Post{
		CreatedAt: now.Add(-10 * time.Minute),
		Likes:     10,
		Replies:   2,
	}
	oldPost := &Post{
		CreatedAt: now.Add(-72 * time.Hour),
		Likes:     10,
		Replies:   2,
	}

	freshScore := rankPost(freshPost, now)
	oldScore := rankPost(oldPost, now)

	if freshScore <= oldScore {
		t.Fatalf("expected fresh score (%f) > old score (%f)", freshScore, oldScore)
	}
}

func TestServerEndpoints(t *testing.T) {
	cfg := Config{
		ActorsPath: "/pds/actors",
		Listen:     ":4040",
		Refresh:    30 * time.Second,
		FeedURI:    "at://did:example/feed/top",
		MaxAge:     30 * 24 * time.Hour,
		Hostname:   "spoonfeed.devsky.app",
		ServiceDID: "did:web:spoonfeed.devsky.app",
	}

	s := &Server{cfg: cfg}
	idx := &Index{
		Posts: map[string]*Post{
			"at://did:plc:1/app.bsky.feed.post/1": {
				URI:       "at://did:plc:1/app.bsky.feed.post/1",
				CreatedAt: time.Now().Add(-1 * time.Hour),
				Likes:     5,
				Replies:   1,
				Score:     10.0,
			},
			"at://did:plc:2/app.bsky.feed.post/2": {
				URI:       "at://did:plc:2/app.bsky.feed.post/2",
				CreatedAt: time.Now().Add(-2 * time.Hour),
				Likes:     2,
				Replies:   0,
				Score:     2.0,
			},
		},
	}
	s.index.Store(idx)

	// Test /healthz
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	s.health(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", w.Code, http.StatusOK)
	}

	var healthRes map[string]any
	if err := json.NewDecoder(w.Body).Decode(&healthRes); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if healthRes["ok"] != true || healthRes["posts"] != float64(2) {
		t.Fatalf("unexpected health response: %v", healthRes)
	}

	// Test /.well-known/did.json
	req = httptest.NewRequest("GET", "/.well-known/did.json", nil)
	w = httptest.NewRecorder()
	s.wellKnownDID(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("wellKnownDID status = %d, want %d", w.Code, http.StatusOK)
	}

	var didDoc DIDDocument
	if err := json.NewDecoder(w.Body).Decode(&didDoc); err != nil {
		t.Fatalf("decode didDoc: %v", err)
	}
	if didDoc.ID != "did:web:spoonfeed.devsky.app" {
		t.Fatalf("unexpected didDoc ID: %s", didDoc.ID)
	}
	if len(didDoc.Service) != 1 || didDoc.Service[0].ServiceEndpoint != "https://spoonfeed.devsky.app" {
		t.Fatalf("unexpected didDoc Service: %+v", didDoc.Service)
	}

	// Test /xrpc/app.bsky.feed.describeFeedGenerator
	req = httptest.NewRequest("GET", "/xrpc/app.bsky.feed.describeFeedGenerator", nil)
	w = httptest.NewRecorder()
	s.describeFeedGenerator(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("describe status = %d, want %d", w.Code, http.StatusOK)
	}

	var descRes describeFeedGeneratorResponse
	if err := json.NewDecoder(w.Body).Decode(&descRes); err != nil {
		t.Fatalf("decode describe: %v", err)
	}
	if descRes.DID != "did:web:spoonfeed.devsky.app" {
		t.Fatalf("expected describe did to be did:web:spoonfeed.devsky.app, got %v", descRes.DID)
	}
	if len(descRes.Feeds) != 1 || descRes.Feeds[0].URI != "at://did:example/feed/top" {
		t.Fatalf("unexpected describe feeds: %+v", descRes.Feeds)
	}

	// Test /xrpc/app.bsky.feed.getFeedSkeleton
	req = httptest.NewRequest("GET", "/xrpc/app.bsky.feed.getFeedSkeleton?feed=at://did:example/feed/top&limit=1", nil)
	w = httptest.NewRecorder()
	s.getFeedSkeleton(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("getFeedSkeleton status = %d, want %d", w.Code, http.StatusOK)
	}

	var skelRes skeletonResponse
	if err := json.NewDecoder(w.Body).Decode(&skelRes); err != nil {
		t.Fatalf("decode skeleton: %v", err)
	}
	if len(skelRes.Feed) != 1 {
		t.Fatalf("expected 1 item, got %d", len(skelRes.Feed))
	}
	if skelRes.Feed[0].Post != "at://did:plc:1/app.bsky.feed.post/1" {
		t.Fatalf("expected top post first, got %s", skelRes.Feed[0].Post)
	}
	if skelRes.Cursor == "" {
		t.Fatalf("expected next cursor to be present")
	}
}

func TestBuildIndexWithPDS(t *testing.T) {
	if _, err := os.Stat("/pds/actors"); os.IsNotExist(err) {
		t.Skip("/pds/actors does not exist")
	}

	idx, err := buildIndex("/pds/actors", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("buildIndex error: %v", err)
	}

	t.Logf("Indexed %d posts from /pds/actors", len(idx.Posts))
}

func TestFindStoresEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	stores, err := findStores(tmpDir)
	if err != nil {
		t.Fatalf("findStores failed: %v", err)
	}
	if len(stores) != 0 {
		t.Fatalf("expected 0 stores, got %d", len(stores))
	}

	// Create a dummy structure
	actorDir := filepath.Join(tmpDir, "ab", "did:plc:testactor")
	if err := os.MkdirAll(actorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actorDir, "store.sqlite"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	stores, err = findStores(tmpDir)
	if err != nil {
		t.Fatalf("findStores failed: %v", err)
	}
	if len(stores) != 1 {
		t.Fatalf("expected 1 store, got %d", len(stores))
	}
}

func TestLoadEnv(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")
	content := `
# Comment line
TEST_KEY_1=simple_value
TEST_KEY_2="quoted value"
TEST_KEY_3='single quoted'
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := loadEnv(envPath); err != nil {
		t.Fatalf("loadEnv failed: %v", err)
	}

	if val := os.Getenv("TEST_KEY_1"); val != "simple_value" {
		t.Errorf("expected simple_value, got %q", val)
	}
	if val := os.Getenv("TEST_KEY_2"); val != "quoted value" {
		t.Errorf("expected 'quoted value', got %q", val)
	}
	if val := os.Getenv("TEST_KEY_3"); val != "single quoted" {
		t.Errorf("expected 'single quoted', got %q", val)
	}
}
