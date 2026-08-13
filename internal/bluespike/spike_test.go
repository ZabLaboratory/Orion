// Package bluespike proves that github.com/ZabLaboratory/Blue/runtime/go is
// importable and executable as a private cross-repo Go dependency from
// Orion. It is isolated from the production runtime — see issue #331.
package bluespike

import (
	"os"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
)

func TestBlueRuntimeLifecycle(t *testing.T) {
	program, err := os.ReadFile("testdata/01-minimal.program.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	runtime := blueruntime.NewRuntime()

	handle, err := runtime.Load(program)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	instance, err := runtime.Start(handle, blueruntime.StartOptions{
		InstanceID: "orion-spike-331",
		Mode:       blueruntime.Preview,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	step, err := runtime.Step(instance)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if step.Status != "idle" {
		t.Fatalf("Step: expected status idle after on-start entrypoint, got %q", step.Status)
	}

	if err := runtime.Stop(instance, "spike-complete"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
