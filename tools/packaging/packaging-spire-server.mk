#remove leading characters since package version expects to start with digit
SPIRE_PACKAGE_VERSION = 1.9.1
SPIRE_OUT_DIR = $(shell pwd)/out_spire/linux_${TARGET_ARCH}

# Creates the proxy debian packages. BUILD_WITH_CONTAINER=1 or in CI/CD (BUILD_WITH_CONTAINER=0)
spire-server_deb: ${SPIRE_OUT_DIR}/spire-server.deb
spire-server_rpm: ${SPIRE_OUT_DIR}/spire-server.rpm


# fpm likes to add extremely high levels of compression. This is fine for release, but for local runs
# where we are just pushing to a local registry (compressed again!), it adds ~1min to builds.
ifneq ($(FAST_VM_BUILDS),)
DEB_COMPRESSION=--deb-compression=none
RPM_COMPRESSION=--rpm-compression=none
endif

SPIRE_SERVER_DEB_BIN=/usr/local/bin

SPIRE_SERVER_DEB_DEPS:=spire-server
SPIRE_SERVER_FILES:=
$(foreach DEP,$(SPIRE_SERVER_DEB_DEPS),\
        $(eval ${SPIRE_OUT_DIR}/spire-server.deb: $(SPIRE_OUT_DIR)/$(DEP)) \
        $(eval ${SPIRE_OUT_DIR}/spire-server.rpm: $(SPIRE_OUT_DIR)/$(DEP)) \
        $(eval SPIRE_SERVER_FILES+=$(SPIRE_OUT_DIR)/$(DEP)=$(SPIRE_SERVER_DEB_BIN)/$(DEP)) )

${SPIRE_OUT_DIR}/spire-server:
				mkdir -p $(SPIRE_OUT_DIR)
				curl -s -N -L https://github.com/spiffe/spire/releases/download/v$(SPIRE_PACKAGE_VERSION)/spire-$(SPIRE_PACKAGE_VERSION)-linux-${TARGET_ARCH}-musl.tar.gz | tar xz -C $(SPIRE_OUT_DIR)
				cp $(SPIRE_OUT_DIR)/spire-$(SPIRE_PACKAGE_VERSION)/bin/spire-server $(SPIRE_OUT_DIR)/

SPIRE_SERVER_DEB_DEST:=/lib/systemd/system/spire-server.service

$(foreach DEST,$(SPIRE_SERVER_DEB_DEST),\
        $(eval ${SPIRE_OUT_DIR}/spire-server.deb:   tools/packaging/spire/$(notdir $(DEST))) \
        $(eval SPIRE_SERVER_FILES+=${REPO_ROOT}/tools/packaging/spire/$(notdir $(DEST))=$(DEST)))

SPIRE_SERVER_FILES+=${REPO_ROOT}/tools/packaging/spire/server.conf=/var/lib/spire/server.conf

SPIRE_SERVER_AGENT_PACKAGE_NAME ?= spire-server

${SPIRE_OUT_DIR}/spire-server.deb: | ${SPIRE_OUT_DIR} spire-server_deb/fpm
${SPIRE_OUT_DIR}/spire-server.rpm: | ${SPIRE_OUT_DIR} spire-server_rpm/fpm

# Package the sidecar rpm file.
spire-server_rpm/fpm: ${SPIRE_OUT_DIR}/spire-server
	rm -f ${SPIRE_OUT_DIR}/spire-server.rpm
	fpm -s dir -t rpm -n ${SPIRE_SERVER_AGENT_PACKAGE_NAME} -p ${SPIRE_OUT_DIR}/spire-server.rpm --version $(SPIRE_PACKAGE_VERSION) -f \
		--url https://solo.io  \
		--license Apache \
		--architecture "${TARGET_ARCH}" \
		--vendor solo.io \
		--maintainer solo@solo.io \
		--after-install tools/packaging/spire/postinst-spire-server.sh \
		--description "SPIRE Server" \
		$(RPM_COMPRESSION) \
		$(SPIRE_SERVER_FILES)

# Package the sidecar deb file.
spire-server_deb/fpm: ${SPIRE_OUT_DIR}/spire-server
	rm -f ${SPIRE_OUT_DIR}/spire-server.deb
	fpm -s dir -t deb -n ${SPIRE_SERVER_AGENT_PACKAGE_NAME} -p ${SPIRE_OUT_DIR}/spire-server.deb --version $(SPIRE_PACKAGE_VERSION) -f \
		--url http://solo.io  \
		--license Apache \
		--vendor solo.io \
		--architecture "${TARGET_ARCH}" \
		--maintainer solo@solo.io \
		--after-install tools/packaging/spire/postinst-spire-server.sh \
		--description "SPIRE Server" \
		$(DEB_COMPRESSION) \
		$(SPIRE_SERVER_FILES)

.PHONY: \
	spire-server_deb \
	spire-server_deb/fpm \
	spire-server_rpm/fpm \
	spire-server_rpm-7/fpm
