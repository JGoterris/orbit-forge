.PHONY: build link test
build:
	go build -o orbit-forge .

# builds it and uses this folder as the plugin, so every change is seen after a restart of orbit
link: build
	orbit plugin link .

test:
	go test -race ./...
