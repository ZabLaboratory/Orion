package lsdp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestZabCamCredsFetcher_NoScopedTokenSkipsRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)

	var gotPaths []string
	fetcher := NewZabCamCredsFetcher(server.URL, func(paths []string) string {
		gotPaths = append([]string(nil), paths...)
		return ""
	}, nil)

	if _, ok := fetcher.FetchViewerCreds(context.Background(), "alice"); ok {
		t.Fatal("FetchViewerCreds reported success without a scoped token")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("anonymous viewer-credentials requests = %d, want 0", got)
	}
	if want := []string{credsTokenScope}; !reflect.DeepEqual(gotPaths, want) {
		t.Fatalf("mint paths = %v, want %v", gotPaths, want)
	}
}

func TestZabCamCredsFetcher_NilMinterSkipsRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)

	fetcher := NewZabCamCredsFetcher(server.URL, nil, nil)
	if _, ok := fetcher.FetchViewerCreds(context.Background(), "alice"); ok {
		t.Fatal("FetchViewerCreds reported success with a nil minter")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("anonymous viewer-credentials requests = %d, want 0", got)
	}
}

func TestZabCamCredsFetcher_ScopedTokenIsRequiredAndSent(t *testing.T) {
	const token = "scoped-viewer-token"
	var requests atomic.Int32
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"meet_room_id": "room-1",
			"meet_token":   "receive-only-token",
			"meet_ws_url":  "wss://meet.example/room-1",
		})
	}))
	t.Cleanup(server.Close)

	var gotPaths []string
	fetcher := NewZabCamCredsFetcher(server.URL, func(paths []string) string {
		gotPaths = append([]string(nil), paths...)
		return token
	}, nil)

	room, ok := fetcher.FetchViewerCreds(context.Background(), "alice/camera")
	if !ok {
		t.Fatal("FetchViewerCreds failed with a scoped token")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("authenticated viewer-credentials requests = %d, want 1", got)
	}
	if got, want := gotPath, "/cam/api/v1/cam/cameras/alice%2Fcamera/credentials"; got != want {
		t.Fatalf("request path = %q, want %q", got, want)
	}
	if got, want := gotAuth, "Bearer "+token; got != want {
		t.Fatalf("authorization = %q, want %q", got, want)
	}
	if want := []string{credsTokenScope}; !reflect.DeepEqual(gotPaths, want) {
		t.Fatalf("mint paths = %v, want %v", gotPaths, want)
	}
	wantRoom := ViewerRoom{SignalingURL: "wss://meet.example/room-1", RoomID: "room-1", JoinToken: "receive-only-token"}
	if !reflect.DeepEqual(room, wantRoom) {
		t.Fatalf("room = %+v, want %+v", room, wantRoom)
	}
}
