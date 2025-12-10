CURRENT_DIR := $(shell pwd)
MODEL?=gpt-4o-mini
API_KEY?=
BASE_URL?=https://api.openai.com/v1
LOG_LEVEL?=debug

run-docker:
	docker build -t aish .
	docker run -it -e LOG_LEVEL=$(LOG_LEVEL) -e MODEL=$(MODEL) -e API_KEY=$(API_KEY) -e BASE_URL=$(BASE_URL) --rm aish
