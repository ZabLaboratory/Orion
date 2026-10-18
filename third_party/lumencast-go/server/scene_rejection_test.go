package server_test

import (
	"testing"

	"github.com/Lumencast/lumencast-go/protocol"
)

// A scene whose backing bundle failed validation serves its rejection
// in place of the snapshot (RFC-0001 : an `x-zab.capture` the server
// refuses must not surface as a healthy, empty scene).
func TestSubscribe_RejectedSceneServesErrorNotSnapshot(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("bad-bundle")
	scene.Reject(protocol.CodeInvalidValue, "`x-zab.deviceRef` \"video:0\" MUST be a logical alias")
	if err := srv.SetActive("bad-bundle"); err != nil {
		t.Fatal(err)
	}

	c := dial(t, url)
	send(t, c, &protocol.Subscribe{Token: "op-tok"})

	frame := recv(t, c)
	errFrame, ok := frame.(*protocol.Error)
	if !ok {
		t.Fatalf("got %T, want *Error", frame)
	}
	if errFrame.Code != string(protocol.CodeInvalidValue) {
		t.Fatalf("got code %q, want INVALID_VALUE", errFrame.Code)
	}
	if errFrame.Recoverable {
		t.Fatal("a rejected bundle is not recoverable")
	}
	if errFrame.Seq != 1 {
		t.Fatalf("got seq %d, want 1", errFrame.Seq)
	}
}

// An unrejected scene keeps serving snapshots — the seam is inert until
// Reject is called.
func TestSubscribe_UnrejectedSceneStillSnapshots(t *testing.T) {
	srv, url := startTestServer(t, opAuth())
	scene := srv.NewScene("good")
	if scene.Rejection() != nil {
		t.Fatal("fresh scene must carry no rejection")
	}
	if err := srv.SetActive("good"); err != nil {
		t.Fatal(err)
	}

	c := dial(t, url)
	send(t, c, &protocol.Subscribe{Token: "op-tok"})
	if _, ok := recv(t, c).(*protocol.Snapshot); !ok {
		t.Fatal("want snapshot on a healthy scene")
	}
}
