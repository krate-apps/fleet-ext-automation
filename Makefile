.PHONY: build pack
build:
	mkdir -p bin
	go build -trimpath -o bin/automation-worker ./cmd/automation-worker

pack:
	bash scripts/pack.sh dist
