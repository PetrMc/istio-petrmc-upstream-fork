#remove leading characters since package version expects to start with digit
SPIRE_PACKAGE_VERSION = 1.9.1
SPIRE_OUT_DIR = $(shell pwd)/out_spire/linux_${TARGET_ARCH}

# Creates the proxy debian packages. BUILD_WITH_CONTAINER=1 or in CI/CD (BUILD_WITH_CONTAINER=0)
spire-agent_deb: ${SPIRE_OUT_DIR}/spire-agent.deb
spire-agent_rpm: ${SPIRE_OUT_DIR}/spire-agent.rpm


# fpm likes to add extremely high levels of compression. This is fine for release, but for local runs
# where we are just pushing to a local registry (compressed again!), it adds ~1min to builds.
ifneq ($(FAST_VM_BUILDS),)
DEB_COMPRESSION=--deb-compression=none
RPM_COMPRESSION=--rpm-compression=none
endif

SPIRE_AGENT_DEB_BIN=/usr/local/bin

SPIRE_AGENT_DEB_DEPS:=spire-agent
SPIRE_AGENT_FILES:=
$(foreach DEP,$(SPIRE_AGENT_DEB_DEPS),\
        $(eval ${SPIRE_OUT_DIR}/spire-agent.deb: $(SPIRE_OUT_DIR)/$(DEP)) \
        $(eval ${SPIRE_OUT_DIR}/spire-agent.rpm: $(SPIRE_OUT_DIR)/$(DEP)) \
        $(eval SPIRE_AGENT_FILES+=$(SPIRE_OUT_DIR)/$(DEP)=$(SPIRE_AGENT_DEB_BIN)/$(DEP)) )

${SPIRE_OUT_DIR}/spire-agent:
				mkdir -p $(SPIRE_OUT_DIR)
				curl -s -N -L https://github.com/spiffe/spire/releases/download/v$(SPIRE_PACKAGE_VERSION)/spire-$(SPIRE_PACKAGE_VERSION)-linux-${TARGET_ARCH}-musl.tar.gz | tar xz -C $(SPIRE_OUT_DIR)
				cp $(SPIRE_OUT_DIR)/spire-$(SPIRE_PACKAGE_VERSION)/bin/spire-agent $(SPIRE_OUT_DIR)/

SPIRE_AGENT_DEB_DEST:=/lib/systemd/system/spire-agent.service

$(foreach DEST,$(SPIRE_AGENT_DEB_DEST),\
        $(eval ${SPIRE_OUT_DIR}/spire-agent.deb:   tools/packaging/spire/$(notdir $(DEST))) \
        $(eval SPIRE_AGENT_FILES+=${REPO_ROOT}/tools/packaging/spire/$(notdir $(DEST))=$(DEST)))

SPIRE_AGENT_FILES+=${REPO_ROOT}/tools/packaging/spire/agent.conf=/var/lib/spire/agent.conf

SPIRE_AGENT_AGENT_PACKAGE_NAME ?= spire-agent

${SPIRE_OUT_DIR}/spire-agent.deb: | ${SPIRE_OUT_DIR} spire-agent_deb/fpm
${SPIRE_OUT_DIR}/spire-agent.rpm: | ${SPIRE_OUT_DIR} spire-agent_rpm/fpm

# Package the sidecar rpm file.
spire-agent_rpm/fpm: ${SPIRE_OUT_DIR}/spire-agent
	rm -f ${SPIRE_OUT_DIR}/spire-agent.rpm
	fpm -s dir -t rpm -n ${SPIRE_AGENT_AGENT_PACKAGE_NAME} -p ${SPIRE_OUT_DIR}/spire-agent.rpm --version $(SPIRE_PACKAGE_VERSION) -f \
		--url https://solo.io  \
		--license Apache \
		--architecture "${TARGET_ARCH}" \
		--vendor solo.io \
		--maintainer solo@solo.io \
		--after-install tools/packaging/spire/postinst-spire-agent.sh \
		--description "SPIRE Agent" \
		$(RPM_COMPRESSION) \
		$(SPIRE_AGENT_FILES)

# Package the sidecar deb file.
spire-agent_deb/fpm: ${SPIRE_OUT_DIR}/spire-agent
	rm -f ${SPIRE_OUT_DIR}/spire-agent.deb
	fpm -s dir -t deb -n ${SPIRE_AGENT_AGENT_PACKAGE_NAME} -p ${SPIRE_OUT_DIR}/spire-agent.deb --version $(SPIRE_PACKAGE_VERSION) -f \
		--url http://solo.io  \
		--license Apache \
		--vendor solo.io \
		--architecture "${TARGET_ARCH}" \
		--maintainer solo@solo.io \
		--after-install tools/packaging/spire/postinst-spire-agent.sh \
		--description "SPIRE Agent" \
		$(DEB_COMPRESSION) \
		$(SPIRE_AGENT_FILES)

.PHONY: \
	spire-agent_deb \
	spire-agent_deb/fpm \
	spire-agent_rpm/fpm \
	spire-agent_rpm-7/fpm
