package gat

// ExportSplitProtoPackage is a test hook exposing splitProtoPackage.
// Lives in a _test.go file so it doesn't leak into the public API.
var ExportSplitProtoPackage = splitProtoPackage
