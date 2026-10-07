# Durable stream control

`store.go` owns `stream-control.lsml`: published global Blue programs/digests
and desired `{running,on_air}` state per local overlay app. It verifies the
document digest at startup and replaces the file atomically. Scene content,
mutated scene LSML, credentials and transient Blue variables never enter it.

`cmd/orion/main.go` restores saved rules directly into RulePlane, then reapplies
the saved overlay intent. No upstream compile is needed on restart. Rule
promotion persists only after load and first step succeed; demotion removes it.

`apps.go` reads an explicitly configured local command manifest. It accepts
either `{app_id:{executable,args,directory}}` or the existing version-1 installed
overlay declaration `{version:1,apps:[{id,exe_path,args,...}]}`. Paths are absolute,
arguments are an array and no shell is used. Blue supplies only an app identifier
and booleans. Unknown apps and failed launches surface as errors.

`controller.go` persists intent before acting, maintains actual PID/running/error
status and restarts still-enabled exited processes. Stop/OFF terminates only an
owned child; Windows also reaps its child tree. Orion cancels and joins the
reconciliation loop before closing its apps. Desired enabled state survives
shutdown. `GET /api/v1/show/overlay-apps` exposes actual process status to operators.
Window capture/on-air composition remains the existing host's responsibility.

Configuration: `ORION_STREAM_INTENT_PATH` overrides the LSML location. Otherwise
it resides alongside the local service-token state, then under the local artifact
root or asset root. `ORION_OVERLAY_APPS_PATH` selects the installed command manifest;
an unset path declares no executable. No product-specific app is hardcoded.

Tests cover restart/digest rejection, process idempotence, OFF/reaping and an
opt-in real Electron Marker rule/capture/native-ACK proof. The package is owned
by Orion; Prism and Lumencast need no edits for this local implementation.
