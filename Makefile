BINARY   := bin/pixsim
PKG      := ./...
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
COVERAGE_MIN := 80

.PHONY: all build run test race cover fmt fmt-check vet lint docker compose-up compose-down clean workspace

all: lint race build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/pixsim

run:
	go run ./cmd/pixsim -scenarios 'scenarios/*.json' -log-format text

test:
	go test $(PKG)

race:
	go test -race $(PKG)

cover:
	go test -race -coverprofile=coverage.out $(PKG)
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -func=coverage.out | awk '/^total:/ { sub("%","",$$3); if ($$3+0 < $(COVERAGE_MIN)) { print "coverage below $(COVERAGE_MIN)%"; exit 1 } }'

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run make fmt" && exit 1)

vet:
	go vet $(PKG)

lint: fmt-check vet

docker:
	docker build --build-arg VERSION=$(VERSION) -t pix-psp-simulator:$(VERSION) .

compose-up:
	docker compose up --build

compose-down:
	docker compose down

# Develop against a local checkout of brpay (go.work is git-ignored).
workspace:
	test -f go.work || (go work init . && go work use ../brpay)

clean:
	rm -rf bin coverage.out
