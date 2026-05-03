.PHONY: all fmt fmt-check vet test

all: fmt-check vet test

fmt:
	gofmt -w $(shell find . -name '*.go')

fmt-check:
	@diff=$$(gofmt -d $$(find . -name '*.go') 2>/dev/null); \
	if [ -n "$$diff" ]; then \
	  echo "gofmt diff (run 'make fmt'):"; echo "$$diff"; exit 1; \
	fi

vet:
	go vet ./...

test:
	go test ./...
