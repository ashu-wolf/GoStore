.PHONY: run test check bench

run:
	go run ./cmd/server

test:
	go test ./...

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race ./...

bench:
	go run ./cmd/bench -data-dir=/var/tmp
