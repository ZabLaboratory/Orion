package streamcontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Commands are installed/local configuration, never supplied by a Blue request.
type Command struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
	Directory  string   `json:"directory"`
}
type AppStatus struct {
	Running        bool   `json:"running_actual"`
	DesiredRunning bool   `json:"running"`
	DesiredOnAir   bool   `json:"on_air"`
	PID            int    `json:"pid,omitempty"`
	Error          string `json:"last_error,omitempty"`
}
type process struct {
	cmd  *exec.Cmd
	done chan struct{}
}
type Apps struct {
	mu        sync.Mutex
	commands  map[string]Command
	processes map[string]*process
	status    map[string]AppStatus
}

func NewApps(manifest string) (*Apps, error) {
	a := &Apps{commands: map[string]Command{}, processes: map[string]*process{}, status: map[string]AppStatus{}}
	if manifest == "" {
		return a, nil
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	var existing struct {
		Version int `json:"version"`
		Apps    []struct {
			ID         string   `json:"id"`
			Executable string   `json:"exe_path"`
			Args       []string `json:"args"`
		} `json:"apps"`
	}
	if json.Unmarshal(raw, &existing) == nil && existing.Version == 1 && existing.Apps != nil {
		for _, entry := range existing.Apps {
			if _, duplicate := a.commands[entry.ID]; duplicate {
				return nil, fmt.Errorf("OVERLAY_COMMAND_DUPLICATE: %s", entry.ID)
			}
			a.commands[entry.ID] = Command{Executable: entry.Executable, Args: entry.Args}
		}
	} else if err := json.Unmarshal(raw, &a.commands); err != nil {
		return nil, err
	}
	for id, command := range a.commands {
		if id == "" || !filepath.IsAbs(command.Executable) || (command.Directory != "" && !filepath.IsAbs(command.Directory)) {
			return nil, fmt.Errorf("OVERLAY_COMMAND_INVALID: %s", id)
		}
	}
	return a, nil
}
func (a *Apps) Status() map[string]AppStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := map[string]AppStatus{}
	for id, status := range a.status {
		result[id] = status
	}
	return result
}
func (a *Apps) Set(ctx context.Context, id string, running bool) error {
	a.mu.Lock()
	p := a.processes[id]
	if !running {
		if p == nil {
			a.status[id] = AppStatus{}
			a.mu.Unlock()
			return nil
		}
		delete(a.processes, id)
		a.mu.Unlock()
		_ = terminateProcess(p.cmd)
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	if p != nil {
		a.mu.Unlock()
		return nil
	}
	command, ok := a.commands[id]
	if !ok {
		a.status[id] = AppStatus{Error: "OVERLAY_APPLICATION_NOT_CONFIGURED"}
		a.mu.Unlock()
		return fmt.Errorf("OVERLAY_APPLICATION_NOT_CONFIGURED: %s", id)
	}
	cmd := exec.Command(command.Executable, command.Args...)
	cmd.Dir = command.Directory
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		a.status[id] = AppStatus{Error: err.Error()}
		a.mu.Unlock()
		return err
	}
	p = &process{cmd: cmd, done: make(chan struct{})}
	a.processes[id] = p
	a.status[id] = AppStatus{Running: true, PID: cmd.Process.Pid}
	a.mu.Unlock()
	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		if a.processes[id] == p {
			delete(a.processes, id)
			status := AppStatus{}
			if err != nil {
				status.Error = err.Error()
			}
			a.status[id] = status
		} else if a.status[id].PID == cmd.Process.Pid {
			a.status[id] = AppStatus{}
		}
		a.mu.Unlock()
		close(p.done)
	}()
	// Detect immediate launcher failure without confusing successful exec with a
	// child already gone. This still reports process status, not rendered pixels.
	select {
	case <-p.done:
		return fmt.Errorf("OVERLAY_APPLICATION_EXITED: %s", id)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}
func (a *Apps) Close() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.processes))
	for id := range a.processes {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range ids {
		_ = a.Set(ctx, id, false)
	}
}
