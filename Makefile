BINARY := bin/wiseyolo

.PHONY: build test fmt vet clean eval-mock eval-live doctor-mock ci

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

# `eval-live` runs the eval harness against the live Jev backend (architecture §7,
# layer 3). It needs a Jev key in the environment; without one it prints a warning
# and skips safely (exit 0, no network call).
eval-live: build
	@if [ -z "$$WISE_YOLO_JEV_API_KEY" ] && [ -z "$$TYPESAFE_API_KEY" ]; then \
		echo "eval-live: no WISE_YOLO_JEV_API_KEY/TYPESAFE_API_KEY set — skipping (opt-in live run)"; \
	else \
		$(BINARY) eval --backend jev --compare; \
	fi

# Health check against the offline backend; part of `ci`.
doctor-mock: build
	$(BINARY) doctor --backend mock

# Full offline verification: formatting, vet, build, tests, eval harness over the
# mock backend, and the doctor health check. Every gate in one target.
ci: fmt vet build test eval-mock doctor-mock
