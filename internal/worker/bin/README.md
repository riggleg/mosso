`make worker` cross-compiles `cmd/mosso-worker` for the sandbox
(linux/amd64) into this directory as `mosso-worker`, where the Mosso server
embeds it (`internal/worker/binary.go`). The binary is git-ignored. Without
it, a server started from the source tree builds the worker with `go build`
the first time a workspace needs it.
