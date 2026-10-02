BINARY := bin/wiseyolo

.PHONY: build test fmt vet clean

# `cmd/wiseyolo` lands in T03; until then `make build` compiles the tree only.
build:
	if [ -d cmd/wiseyolo ]; then go build -o $(BINARY) ./cmd/wiseyolo; else go build ./...; fi

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -rf bin scratch
