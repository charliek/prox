.PHONY: build test test-race lint clean install

build:
	go build -o prox ./cmd/prox

test:
	go test -timeout 20m -v ./...

test-race:
	go test -timeout 20m -race ./...

lint:
	golangci-lint run

clean:
	rm -f prox

install: build
	mkdir -p ~/.local/bin
	cp prox ~/.local/bin/prox
	@if [ "$$(uname)" = "Linux" ]; then sudo setcap 'cap_net_bind_service=+ep' ~/.local/bin/prox; fi
	@if [ "$$(uname)" = "Darwin" ]; then codesign --force --sign - ~/.local/bin/prox; fi
