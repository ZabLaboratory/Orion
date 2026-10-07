// Package lsdpreception connects to the single native receiver owned by Prism.
// Orion verifies it but never launches or terminates that shared process.
package lsdpreception

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"

	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

type Reception struct{ Address, Resource string }

func New(address, resource string) (*Reception, error) {
	host, port, err := net.SplitHostPort(address)
	number, numberErr := strconv.Atoi(port)
	if err != nil || numberErr != nil || number < 1 || number > 65535 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || resource != "orion/state" {
		return nil, errors.New("LSDP_RECEPTION_ENDPOINT_INVALID")
	}
	return &Reception{Address: address, Resource: resource}, nil
}

// Check performs the native handshake, checks capabilities, then reads Orion's
// actual resource. A listening port alone is not proof of a functioning server.
func (r *Reception) Check(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	client, err := native.Dial(ctx, r.Address, "", nil)
	if err != nil {
		return err
	}
	defer client.Close()
	response, err := client.Exchange(ctx, map[string]any{"kind": "capabilities"})
	if err != nil {
		return err
	}
	var capabilities struct {
		Ready     bool
		Profiles  []string
		Resources []string
	}
	if err = json.Unmarshal(response, &capabilities); err != nil {
		return err
	}
	has := func(items []string, value string) bool {
		for _, item := range items {
			if item == value {
				return true
			}
		}
		return false
	}
	if !capabilities.Ready || !has(capabilities.Resources, r.Resource) || !has(capabilities.Profiles, "lsdp.node.routing/1") || !has(capabilities.Profiles, "lsdp.state.application/1") {
		return errors.New("LSDP_RECEPTION_CAPABILITIES_INVALID")
	}
	_, err = client.Exchange(ctx, map[string]any{"kind": "state.read", "target": r.Resource})
	for _, target := range []string{"solar/program", "solar/preview", "solar/generations", "solar/sessions"} {
		if !has(capabilities.Resources, target) {
			return errors.New("LSDP_RECEPTION_SCENE_RESOURCES_MISSING")
		}
	}
	return err
}
