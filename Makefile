.PHONY: test check run build worker

test:
	go test ./...

check:
	go fmt ./...
	go vet ./...
	go test ./...

# The worker runs inside Vercel Sandboxes (linux/amd64); the server embeds
# it and uploads it into each workspace's sandbox.
worker:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o internal/worker/bin/mosso-worker ./cmd/mosso-worker

run: worker
	go run ./cmd/mosso

build: worker
	mkdir -p bin
	go build -o bin/mosso ./cmd/mosso
	go build -o bin/mosso-worker ./cmd/mosso-worker
