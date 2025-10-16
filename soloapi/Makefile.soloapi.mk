
export REPO_ROOT := $(shell git rev-parse --show-toplevel)

.PHONY: gen-soloapi
gen-soloapi:
	$(REPO_ROOT)/soloapi/generate.sh
