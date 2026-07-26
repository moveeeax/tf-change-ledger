BINARY := tf-change-ledger
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race vet fmt check clean demo

all: check build

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/$(BINARY)

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: vet test
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi

# Runs the flow documented in the README against the committed fixtures.
demo: build
	./$(BINARY) summarize -plan testdata/plan-destructive.json -config testdata/.tf-ledger.yml -author alice -dir envs/prod

clean:
	rm -f $(BINARY)
