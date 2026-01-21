.PHONY: build test lint clean

build:
	go build -o prox ./cmd/prox

test:
	go test -v ./...

lint:
	golangci-lint run

clean:
	rm -f prox

install: build
	mkdir -p ~/.local/bin
	cp prox ~/.local/bin/prox
