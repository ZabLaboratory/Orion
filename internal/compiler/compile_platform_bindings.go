package compiler

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Platform-event leaf binding (ADR 003 §3.3.3, issue #84).
//
// platformLeafPrefix is the global namespace Quasar writes into. Leaves
// under it are NEVER blueprint-key-prefixed (prefixGraphNodes skips
// them): the address is a cross-repo contract — Blue declares it,
// Quasar computes it, Orion must expand to the byte-identical string —
// so it cannot vary with a scene-local blueprint key.
const platformLeafPrefix = "__inputs.platform."

// platformChannelRE is the canonical channel charset (ADR 005 §9 /
// ADR 003 §3.3). Validation runs AFTER casefolding: pure-case variants
// of a valid handle are folded, anything else is rejected — never
// rewritten. The regex guarantees the folded channel is ASCII, on which
// Go's strings.ToLower and Python's str.lower agree byte-for-byte, so
// Orion's expansion matches Quasar's.
var platformChannelRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// platformNodeRef reports whether compute names a quasar platform-event
// node (`quasar.<platform>.<event>@<version>`) and, if so, returns its
// platform and event-type segments. The event list itself is owned by
// Blue's manifest (CANONICAL_EVENT_TYPES → the 14 `quasar.twitch.*@1`
// entries today); validateBlueprint's manifest gate has already
// rejected unknown computes before this runs, so no Orion-side
// allowlist is duplicated here.
func platformNodeRef(compute string) (platform, event string, ok bool) {
	rest, found := strings.CutPrefix(compute, "quasar.")
	if !found {
		return "", "", false
	}
	rest, _, _ = strings.Cut(rest, "@")
	platform, event, found = strings.Cut(rest, ".")
	if !found || platform == "" || event == "" {
		return "", "", false
	}
	return platform, event, true
}

// platformLeafPath expands one platform node's authored config.channel
// into the canonical leaf `__inputs.platform.<platform>.<channel>.last_<event>`
// (ADR 003 §3.3.2). Channel handling is casefold-THEN-validate: a
// `ZabChannel` folds to `zabchannel`; a `Zab-Channel` is rejected
// (PLATFORM_CHANNEL_INVALID), not rewritten. Both diagnostics are
// structural authoring errors on the node's config — not capability
// rejections of the node type (§3.3.3).
func platformLeafPath(n BlueprintNode, platform, event string) (string, *Diagnostic) {
	raw, ok := n.Config["channel"]
	if !ok {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelMissing,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s) declares no config.channel — cannot expand its %s<%s>.last_%s leaf", n.ID, n.Compute, platformLeafPrefix, platform, event),
			Path:     n.ID,
		}
	}
	var channel string
	if err := json.Unmarshal(raw, &channel); err != nil {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelInvalid,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s): config.channel must be a JSON string", n.ID, n.Compute),
			Path:     n.ID,
		}
	}
	if channel == "" {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelMissing,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s): config.channel is empty", n.ID, n.Compute),
			Path:     n.ID,
		}
	}
	folded := strings.ToLower(channel)
	if !platformChannelRE.MatchString(folded) {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelInvalid,
			Severity: "error",
			Message:  fmt.Sprintf("platform node %s (%s): config.channel %q is not a valid channel handle (after casefold it must match %s)", n.ID, n.Compute, channel, platformChannelRE.String()),
			Path:     n.ID,
		}
	}
	return platformLeafPrefix + platform + "." + folded + ".last_" + event, nil
}

// platformEventEntryLeaf expands an `on-platform-event` entrypoint node's
// config (platform/channel/event_type) into the canonical platform leaf
// `__inputs.platform.<platform>.<channel>.last_<event_type>` it observes
// (ADR 013 §3). Unlike platformLeafPath (whose platform/event come from the
// `quasar.<platform>.<event>@N` compute name), here all three segments are
// AUTHORED config — but the channel handling is identical (casefold-then-
// validate, same charset, same PLATFORM_CHANNEL_INVALID diagnostic), so the
// leaf an entry observes is byte-identical to the leaf the matching quasar.*
// dataflow input expands to. Diagnostics are structural authoring errors on
// the node's config, never capability rejections of the entry type.
func platformEventEntryLeaf(n BlueprintNode) (string, *Diagnostic) {
	platform, pd := platformEventConfigSegment(n, "platform")
	if pd != nil {
		return "", pd
	}
	eventType, ed := platformEventConfigSegment(n, "event_type")
	if ed != nil {
		return "", ed
	}
	// Channel: reuse platformLeafPath's exact channel discipline by
	// delegating to it (it reads config.channel + applies casefold-then-
	// validate against platformChannelRE), passing the authored platform +
	// `last_<event_type>` so the returned leaf matches the quasar.* form.
	return platformLeafPath(n, platform, eventType)
}

// platformEventConfigSegment reads and validates one required lowercase
// path segment (`platform` or `event_type`) from an on-platform-event
// node's config. The charset matches the platform/event segments of the
// quasar.* leaf convention (compile-time ASCII, so Orion's expansion and
// Quasar's producer agree). A missing key reuses ErrPlatformChannelMissing
// shape; a malformed value reuses ErrPlatformChannelInvalid — the same
// structural authoring diagnostics the channel uses.
func platformEventConfigSegment(n BlueprintNode, key string) (string, *Diagnostic) {
	raw, ok := n.Config[key]
	if !ok {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelMissing,
			Severity: "error",
			Message:  fmt.Sprintf("on-platform-event node %s declares no config.%s — cannot expand its %s leaf", n.ID, key, platformLeafPrefix),
			Path:     n.ID,
		}
	}
	var val string
	if err := json.Unmarshal(raw, &val); err != nil {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelInvalid,
			Severity: "error",
			Message:  fmt.Sprintf("on-platform-event node %s: config.%s must be a JSON string", n.ID, key),
			Path:     n.ID,
		}
	}
	if val == "" {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelMissing,
			Severity: "error",
			Message:  fmt.Sprintf("on-platform-event node %s: config.%s is empty", n.ID, key),
			Path:     n.ID,
		}
	}
	folded := strings.ToLower(val)
	if !platformSegmentRE.MatchString(folded) {
		return "", &Diagnostic{
			Code:     ErrPlatformChannelInvalid,
			Severity: "error",
			Message:  fmt.Sprintf("on-platform-event node %s: config.%s %q is not a valid %s segment (after casefold it must match %s)", n.ID, key, val, key, platformSegmentRE.String()),
			Path:     n.ID,
		}
	}
	return folded, nil
}

// platformSegmentRE is the charset for the platform/event_type path
// segments (mirrors the quasar.* convention: a lowercase ascii word that
// may carry underscores). Channel uses platformChannelRE; these two segments
// are author-supplied for an on-platform-event entry, so they are validated
// here with the same fold-then-match discipline.
var platformSegmentRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// platformStreamBindings synthesizes one
// ExternalAdapter{Kind:"platform-stream"} per DISTINCT platform leaf in
// the compiled node set (ADR 003 §3.3.3, normative). The binding is a
// PURE acceptance declaration: it exists so sceneAcceptsPath routes
// Quasar's scoped service-token writes to the scene — without it the
// write is silently absorbed. NO adapter goroutine is ever spawned for
// it (the poller / pg-listen starters filter on their own Kind), and
// none of the goroutine-bearing fields (URL, FrequencyHz, Channel) is
// set. Leaves are sorted for scene_version hash determinism.
//
// entryLeaves (ADR 013 §3.6) is the ADDITIVE set of `__inputs.platform.*`
// leaves carried by `on-platform-event` ExecEntries — the leaf an entry
// observes is borne by ExecEntry.Event, NOT by a quasar.* input GraphNode,
// so it would be missed by the node scan alone. Merging it here means a
// scene with ONLY an arming entry (no dataflow input on that leaf) still
// gets the acceptance binding that routes the Quasar write — and a scene
// with BOTH dedups to one binding (coexistence, §3.6). No new adapter Kind.
func platformStreamBindings(nodes []GraphNode, entryLeaves map[string]struct{}) []ExternalAdapter {
	seen := map[string]struct{}{}
	var leaves []string
	for _, n := range nodes {
		if _, _, ok := platformNodeRef(n.Compute); !ok {
			continue
		}
		if n.Path == "" {
			continue
		}
		if _, dup := seen[n.Path]; dup {
			continue
		}
		seen[n.Path] = struct{}{}
		leaves = append(leaves, n.Path)
	}
	// Merge the on-platform-event entry leaves (ADR 013 §3.6) — additive,
	// deduped against the quasar.* node leaves already collected.
	for leaf := range entryLeaves {
		if leaf == "" {
			continue
		}
		if _, dup := seen[leaf]; dup {
			continue
		}
		seen[leaf] = struct{}{}
		leaves = append(leaves, leaf)
	}
	sort.Strings(leaves)
	out := make([]ExternalAdapter, 0, len(leaves))
	for _, leaf := range leaves {
		out = append(out, ExternalAdapter{
			Key:         leaf,
			Label:       "Quasar platform stream",
			Kind:        "platform-stream",
			TargetPaths: []string{leaf},
		})
	}
	return out
}

// eventTopicBindings synthesizes one ExternalAdapter{Kind:"event-topic"}
// per DISTINCT `__events.*` topic the scene observes (issue #148, ADR 008
// §3.3). It is the exact mirror of platformStreamBindings: a PURE
// acceptance declaration so the inbox's sceneAcceptsPath routes an
// operator/service write to `__events.<topic>` to the scene — without it
// the write is silently absorbed (the gap ADR 008 closes). NO adapter
// goroutine is ever spawned for this Kind (the poller / pg-listen
// starters filter on their own Kind), and none of the goroutine-bearing
// fields (URL, FrequencyHz, Channel) is set. The topic namespace is flat
// and global (the runtime indexes execOnEvent by the raw event name), so
// the leaf is `__events.<topic>` with no blueprint-key prefix. Topics are
// sorted for scene_version hash determinism (criterion #6).
//
// Two observation channels feed it, deduped to one binding per leaf:
//   - topics: bare topic names borne by on-event exec entrypoints (the
//     #148 case). Each is prefixed with `__events.` here.
//   - inputLeaves: FULL `__events.*` leaf addresses borne by pure-dataflow
//     `core.input@1` nodes (the ADR 013 quasar-finale 5th-link case). Already
//     prefixed — they ARE the data node's Path. A spine-less reactive scene
//     reads its topic this way (no on-event entry), so without this merge its
//     write is rejected by sceneAcceptsPath and the cone never wakes.
func eventTopicBindings(topics map[string]struct{}, inputLeaves map[string]struct{}) []ExternalAdapter {
	if len(topics) == 0 && len(inputLeaves) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(topics)+len(inputLeaves))
	var leaves []string
	add := func(leaf string) {
		if leaf == "" {
			return
		}
		if _, dup := seen[leaf]; dup {
			return
		}
		seen[leaf] = struct{}{}
		leaves = append(leaves, leaf)
	}
	for t := range topics {
		add(eventsLeafPrefix + t)
	}
	for leaf := range inputLeaves {
		add(leaf)
	}
	sort.Strings(leaves)
	out := make([]ExternalAdapter, 0, len(leaves))
	for _, leaf := range leaves {
		out = append(out, ExternalAdapter{
			Key:         leaf,
			Label:       "on-event topic",
			Kind:        "event-topic",
			TargetPaths: []string{leaf},
		})
	}
	return out
}

// eventInputLeaves collects the distinct `__events.*` leaf paths borne by
// the compiled DATA nodes — a `core.input@1` whose config.name names an
// event topic (the pure-dataflow reactive scene, ADR 013). These leaves
// are the address a dataflow scene reads its event from WITHOUT an on-event
// exec entry, so they are invisible to the eventTopics scan (fed only by
// ExecEntries) and need the same acceptance binding as an on-event topic.
// The scan runs over the post-prefix `sorted` nodes; `__events.*` is a flat
// global namespace exempt from blueprint-key prefixing (parity with the
// platform-leaf exemption), so the path is the byte-identical address the
// inbox gates on. Mirrors platformStreamBindings' node scan.
func eventInputLeaves(nodes []GraphNode) map[string]struct{} {
	out := map[string]struct{}{}
	for _, n := range nodes {
		if n.Kind != "input" {
			continue
		}
		if strings.HasPrefix(n.Path, eventsLeafPrefix) {
			out[n.Path] = struct{}{}
		}
	}
	return out
}
