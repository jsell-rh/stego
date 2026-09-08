`grpc.go` is from `google.golang.org/grpc/cmd/protoc-gen-go-grpc` v1.6.1,
commit `830c9098e4d034e0c7641e1fbc6001c2570bbc0f`.
Only its package declaration was changed. The adjacent license applies.
`export.go` supplies the default options and an entry point for STEGO.

Source: https://github.com/grpc/grpc-go/tree/830c9098e4d034e0c7641e1fbc6001c2570bbc0f/cmd/protoc-gen-go-grpc

Keep this copy in step with its pinned version. Do not change generated wire
behavior here. The protobuf message generator is a pinned Go module dependency.
