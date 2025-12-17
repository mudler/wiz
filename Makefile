CURRENT_DIR := $(shell pwd)
MODEL?=gpt-4o-mini
API_KEY?=
BASE_URL?=https://api.openai.com/v1
LOG_LEVEL?=debug
GORELEASER?=
GO?=go

# check if goreleaser exists
ifeq (, $(shell which goreleaser))
	GORELEASER=curl -sfL https://goreleaser.com/static/run | bash -s --
else
	GORELEASER=$(shell which goreleaser)
endif

build:
	go build -o wiz .

run-docker:
	docker build -t wiz .
	docker run -it -e LOG_LEVEL=$(LOG_LEVEL) -e MODEL=$(MODEL) -e API_KEY=$(API_KEY) -e BASE_URL=$(BASE_URL) --rm wiz

dev-dist:
	$(GORELEASER) build --snapshot --clean

dist:
	$(GORELEASER) build --clean

test:
	$(GO) test ./...