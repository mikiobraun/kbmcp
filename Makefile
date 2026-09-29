# All targets are phony: go's own build cache decides what actually recompiles,
# and "fmq" would otherwise be mistaken for the fmq/ directory and never run.
.PHONY: build kbmcp fmq install test vet clean

build: kbmcp fmq

kbmcp:
	go build -o kbmcp .

fmq:
	go build -o bin/fmq ./cmd/fmq

# search_frontmatter looks fmq up on PATH; this puts it in $(go env GOPATH)/bin.
install:
	go install ./cmd/fmq

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f kbmcp bin/fmq
