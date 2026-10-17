.PHONY: build run test

build:
	cd aiserver && go build -o radio-ai-server .

test:
	cd aiserver && go test -v ./...

# Serves the pages and /api/* on http://127.0.0.1:8420 (override: RADIO_LISTEN).
run: build
	./aiserver/radio-ai-server
