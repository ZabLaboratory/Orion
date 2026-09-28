package api

import (
	"os"
	"strconv"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/providers"
)

const (
	benchmarkProgramID = "orion-181-chat-overlay"
	benchmarkSceneID   = "orion-181-chat-overlay-benchmark"
	benchmarkDigest    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	benchmarkOrigin    = "quasar.twitch.zablab_chat"
	benchmarkTopic     = "quasar.twitch.chat_message"
)

type benchmarkSceneMirror struct {
	deltas uint64
}

func (m *benchmarkSceneMirror) Forward(message any) {
	if _, ok := message.(*protocol.Delta); ok {
		m.deltas++
	}
}

func benchmarkBlueProgram(b *testing.B) []byte {
	b.Helper()
	program, err := os.ReadFile("testdata/orion_181_chat_overlay_program.json")
	if err != nil {
		b.Fatal(err)
	}
	return program
}

func benchmarkBlueChatEvent(sequence uint64) ([]byte, error) {
	return providers.BuildEvent(
		"blue-bench-"+strconv.FormatUint(sequence, 10),
		benchmarkOrigin,
		benchmarkTopic,
		"orion-181-benchmark",
		sequence,
		1786500000000+int64(sequence),
		map[string]any{"message": "benchmark chat"},
	)
}

// BenchmarkOrion181BlueSceneAdmission measures loading and starting the
// immutable program emitted by Blue's real compiler for the chat-overlay
// scene (Blue/tests/test_orion_181_databound_fixture.py). Each iteration uses
// a fresh Orion host, so this intentionally measures a cold scene admission.
func BenchmarkOrion181BlueSceneAdmission(b *testing.B) {
	program := benchmarkBlueProgram(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		host := bluehost.NewHost()
		instanceID := benchmarkProgramID + "-" + strconv.Itoa(i)
		if err := host.Take(instanceID, benchmarkSceneID, benchmarkDigest, program, nil, nil, nil); err != nil {
			b.Fatalf("take Blue chat scene: %v", err)
		}
		if err := host.Release(bluehost.SlotOnAir, "benchmark-complete"); err != nil {
			b.Fatalf("release Blue chat scene: %v", err)
		}
	}
}

// BenchmarkOrion181BlueChatToProjection measures one real Quasar-shaped chat
// event through Orion's active-platform admission, the Blue runtime, and the
// BlueWire projection sent to the LSDP scene mirror. It excludes Pulsar,
// network I/O, Solar painting, and scene admission; those are separate costs.
func BenchmarkOrion181BlueChatToProjection(b *testing.B) {
	program := benchmarkBlueProgram(b)
	leaf, err := providers.CanonicalPlatformLeaf("twitch", "zablab_chat", "chat_message")
	if err != nil {
		b.Fatal(err)
	}
	host := bluehost.NewHost()
	if err := host.Take("blue-scene-benchmark", benchmarkSceneID, benchmarkDigest, program, nil, nil, nil); err != nil {
		b.Fatalf("take Blue chat scene: %v", err)
	}
	defer func() {
		providers.ResetActiveIngress(host)
		_ = host.Release(bluehost.SlotOnAir, "benchmark-complete")
	}()

	mirror := &benchmarkSceneMirror{}
	bridge := bluewire.NewBridge(
		host,
		bluehost.SlotOnAir,
		mirror,
		benchmarkSceneID,
		benchmarkDigest,
		"blue-scene-benchmark",
		blueproject.TargetProgram,
		"revision-1",
		"orion-181-benchmark",
	)

	// BuildEvent belongs to the upstream producer, not Orion's hot path. Keep
	// valid, uniquely sequenced Quasar-shaped wire events ready before timing.
	events := make([][]byte, b.N)
	for i := range events {
		events[i], err = benchmarkBlueChatEvent(uint64(i + 1))
		if err != nil {
			b.Fatalf("build chat event %d: %v", i+1, err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, event := range events {
		receipt, err := providers.InjectActivePlatform(host, leaf, event)
		if err != nil {
			b.Fatalf("admit chat event: %v", err)
		}
		if receipt.Status != "accepted" {
			b.Fatalf("chat event was not accepted: %s", receipt.Status)
		}
		if err := bridge.TickOnce(1.0 / 30.0); err != nil {
			b.Fatalf("execute and project chat event: %v", err)
		}
	}
	b.StopTimer()
	if mirror.deltas != uint64(b.N) {
		b.Fatalf("projected %d LSDP deltas for %d chat events", mirror.deltas, b.N)
	}
	b.ReportMetric(float64(mirror.deltas)/float64(b.N), "wire-delta/op")
}
