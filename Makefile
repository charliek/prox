.PHONY: build test lint clean install

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
	@if [ "$$(uname)" = "Linux" ]; then sudo setcap 'cap_net_bind_service=+ep' ~/.local/bin/prox; fi
