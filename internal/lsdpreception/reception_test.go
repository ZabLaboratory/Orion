package lsdpreception

import (
	"context"
	"encoding/json"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
	"os"
	"testing"
)

func TestEndpoint(t *testing.T) {
	for _, address := range []string{"example.com:4000", "0.0.0.0:4000", "192.168.1.10:4000", "127.0.0.1:0", "127.0.0.1:99999"} {
		if _, err := New(address, "orion/state"); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	if _, err := New("127.0.0.1:4000", "solar/program"); err == nil {
		t.Fatal("accepted Solar resource")
	}
}

func TestRealSharedNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS")
	if address == "" {
		t.Skip("requires actual shared Rust receiver")
	}
	receiver, err := New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	client, err := native.Dial(context.Background(), address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	before, err := client.Exchange(context.Background(), map[string]any{"kind": "state.read", "target": "orion/state"})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct{ State json.RawMessage }
	if err := json.Unmarshal(before, &snapshot); err != nil {
		t.Fatal(err)
	}
	receipt, err := client.SendPort(context.Background(), "", "orion/state", "orion.state/1", map[string]any{"revision": 91})
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Completed(receipt); err != nil {
		t.Fatal(err)
	}
	receipt, err = client.SendPort(context.Background(), "", "orion/state", "orion.state/1", snapshot.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Completed(receipt); err != nil {
		t.Fatal(err)
	}
}
