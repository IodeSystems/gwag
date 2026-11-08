package controlplanev1

import _ "embed"

// ControlProto is the source of control.proto exactly as on disk. The
// gateway's Schema Registry serves it as the default subject
// gateway.controlplane.v1.ControlPlane.
//
//go:embed control.proto
var ControlProto string
