BINARY  ?= odoo
PREFIX  ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo dev)

LDFLAGS := -X main.VERSION=$(VERSION)

.PHONY: build install uninstall test fmt vet tidy clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "→ installed $(PREFIX)/bin/$(BINARY) ($(VERSION))"

uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
