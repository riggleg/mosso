.PHONY: test check run build

test:
	go test ./...

check:
	go fmt ./...
	go vet ./...
	go test ./...

run:
	go run ./cmd/mosso

build:
	mkdir -p bin
	go build -o bin/mosso ./cmd/mosso
	go build -o bin/mosso-worker ./cmd/mosso-worker
