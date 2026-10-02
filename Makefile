BINARY := bin/wiseyolo

.PHONY: build test fmt vet clean eval-mock

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

# `eval-mock` runs the full eval harness against the offline mock backend:
# no network, report written to reports/, one line appended to
# reports/history.jsonl (architecture §7). Running it is a repo-state change;
# the appended history line is meant to be committed.
eval-mock: build
	$(BINARY) eval --backend mock
