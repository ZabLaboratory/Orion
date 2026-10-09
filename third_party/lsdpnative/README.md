# Lumencast native Go client

client.go is copied unchanged from lumencast-lsdp-native/addons/go/client.go at
e695e14f9f664f430ffc2e0e75d083fab88abc7f, Apache-2.0. Its live consumer is Orion's
native producer, receipt barriers and receiver readiness (internal/lsdpreception). No process launcher
is copied: Prism owns the common daemon. Generic transport APIs remain upstream;
we do not fork the native server or rewrite its protocol in Orion.
