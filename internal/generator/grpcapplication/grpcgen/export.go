package grpcgen

import "google.golang.org/protobuf/compiler/protogen"

const version = "1.6.1"

var requireUnimplemented = newTrue()
var useGenericStreams = newTrue()

func newTrue() *bool { value := true; return &value }

// GenerateFile uses the pinned upstream generator with its default options.
func GenerateFile(plugin *protogen.Plugin, file *protogen.File) { generateFile(plugin, file) }
