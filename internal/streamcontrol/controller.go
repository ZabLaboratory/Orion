package streamcontrol

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Mirror interface{ EmitOverlayApp(string, *bool, *bool) }

// Controller persists intent before executing it and publishes process status
// separately. Only configured local applications can be spawned.
type Controller struct {
	store    *Store
	apps     *Apps
	mirror   Mirror
	mu       sync.Mutex
	failures map[string]error
}

func NewController(store *Store, apps *Apps, mirror Mirror) *Controller {
	return &Controller{store: store, apps: apps, mirror: mirror, failures: map[string]error{}}
}
func (c *Controller) EmitOverlayApp(id string, running, onAir *bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.store.SetApp(id, running, onAir); err != nil {
		c.failures[id] = err
		return
	}
	app := c.store.Snapshot().Apps[id]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.apps.Set(ctx, id, app.Running)
	if err == nil {
		delete(c.failures, id)
	} else {
		c.failures[id] = err
	}
	if c.mirror != nil {
		c.mirror.EmitOverlayApp(id, &app.Running, &app.OnAir)
	}
}
func (c *Controller) Error() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var failures []error
	for _, err := range c.failures {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
func (c *Controller) Status() map[string]AppStatus {
	status := c.apps.Status()
	for id, app := range c.store.Snapshot().Apps {
		value := status[id]
		value.DesiredRunning, value.DesiredOnAir = app.Running, app.OnAir
		status[id] = value
	}
	return status
}

// Run reconciles a still-enabled app after an unexpected exit. Closing Orion
// stops owned processes while the durable enabled intent survives.
func (c *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			intent := c.store.Snapshot()
			status := c.apps.Status()
			for id, app := range intent.Apps {
				if app.Running && !status[id].Running {
					c.EmitOverlayApp(id, &app.Running, &app.OnAir)
				}
			}
		}
	}
}
