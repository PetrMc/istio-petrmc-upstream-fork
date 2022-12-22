#remove leading characters since package version expects to start with digit
PACKAGE_VERSION ?= $(shell echo $(VERSION) | sed 's/^[a-z]*-//' | sed 's/-//')

# Creates the proxy debian packages. BUILD_WITH_CONTAINER=1 or in CI/CD (BUILD_WITH_CONTAINER=0)
# Only in CI env do we copy the packages to the expected directory based on the 'CI' env var
ambient_deb: ${TARGET_OUT_LINUX}/release/istio-ambient.deb
ifeq ($(CI),true)
	mkdir -p ${GOPATH}/../out/deb # In case it doesn't exist yet
# Mirror logic of https://github.com/istio/release-builder/blob/9d9bd7b1bd90e6fc95b09805e7e22dbef16d58d2/pkg/build/debian.go#L49
# except WORK_DIR is not part of the environment variables. GOPATH is defined by release-builder, which is ${WORK_DIR}/work so we can
# use a relative off of that to get to the output directory.
ifeq ($(TARGET_ARCH),amd64)
		cp ${TARGET_OUT_LINUX}/release/istio-ambient.deb ${GOPATH}/../out/deb/
else
		cp ${TARGET_OUT_LINUX}/release/istio-ambient.deb ${GOPATH}/../out/deb/istio-ambient-${TARGET_ARCH}.deb
endif
endif

ambient_rpm: ${TARGET_OUT_LINUX}/release/istio-ambient.rpm
ifeq ($(CI),true)
	mkdir -p ${GOPATH}/../out/rpm # In case it doesn't exist yet
# Mirror logic of https://github.com/istio/release-builder/blob/9d9bd7b1bd90e6fc95b09805e7e22dbef16d58d2/pkg/build/debian.go#L49
# except WORK_DIR is not part of the environment variables. GOPATH is defined by release-builder, which is ${WORK_DIR}/work so we can
# use a relative off of that to get to the output directory.
ifeq ($(TARGET_ARCH),amd64)
	cp ${TARGET_OUT_LINUX}/release/istio-ambient.rpm ${GOPATH}/../out/rpm/
else
	cp ${TARGET_OUT_LINUX}/release/istio-ambient.rpm ${GOPATH}/../out/rpm/istio-ambient-${TARGET_ARCH}.rpm
endif
endif

# fpm likes to add extremely high levels of compression. This is fine for release, but for local runs
# where we are just pushing to a local registry (compressed again!), it adds ~1min to builds.
ifneq ($(FAST_VM_BUILDS),)
DEB_COMPRESSION=--deb-compression=none
RPM_COMPRESSION=--rpm-compression=none
endif

# Base directory for istio binaries. Likely to change !
ISTIO_DEB_AMBIENT_BIN=/usr/local/bin

# Home directory of istio-proxy user. It is symlinked /etc/istio --> /var/lib/istio
ISTIO_PROXY_HOME=/var/lib/istio

AMBIENT_DEB_DEPS:=ztunnel pilot-agent
AMBIENT_FILES:=
$(foreach DEP,$(AMBIENT_DEB_DEPS),\
        $(eval ${TARGET_OUT_LINUX}/release/istio-ambient.deb: $(TARGET_OUT_LINUX)/$(DEP)) \
        $(eval ${TARGET_OUT_LINUX}/release/istio-ambient.rpm: $(TARGET_OUT_LINUX)/$(DEP)) \
        $(eval AMBIENT_FILES+=$(TARGET_OUT_LINUX)/$(DEP)=$(ISTIO_DEB_AMBIENT_BIN)/$(DEP)) )

ISTIO_AMBIENT_DEB_DEST:=${ISTIO_DEB_AMBIENT_BIN}/istio-start.sh \
		/lib/systemd/system/istio.service

$(foreach DEST,$(ISTIO_AMBIENT_DEB_DEST),\
        $(eval ${TARGET_OUT_LINUX}/istio-ambient.deb:   tools/packaging/ambient/$(notdir $(DEST))) \
        $(eval AMBIENT_FILES+=${REPO_ROOT}/tools/packaging/ambient/$(notdir $(DEST))=$(DEST)))

# original name used in 0.2 - will be updated to 'istio.deb' since it now includes all istio binaries.
AMBIENT_PACKAGE_NAME ?= istio-ambient

# Note: adding --deb-systemd ${REPO_ROOT}/tools/packaging/ambient/istio-ambient.service will result in
# a /etc/systemd/system/multi-user.target.wants/istio-ambient.service and auto-start. Currently not used
# since we need configuration.
# --iteration 1 adds a "-1" suffix to the version that didn't exist before
${TARGET_OUT_LINUX}/release/istio-ambient.deb: | ${TARGET_OUT_LINUX} ambient_deb/fpm
${TARGET_OUT_LINUX}/release/istio-ambient.rpm: | ${TARGET_OUT_LINUX} ambient_rpm/fpm

# Package the sidecar rpm file.
ambient_rpm/fpm:
	rm -f ${TARGET_OUT_LINUX}/release/istio-ambient.rpm
	fpm -s dir -t rpm -n ${AMBIENT_PACKAGE_NAME} -p ${TARGET_OUT_LINUX}/release/istio-ambient.rpm --version $(PACKAGE_VERSION) -f \
		--url http://istio.io  \
		--license Apache \
		--architecture "${TARGET_ARCH}" \
		--vendor istio.io \
		--maintainer istio@istio.io \
		--after-install tools/packaging/postinst-ambient.sh \
		--description "Istio Ambient" \
		--depends iproute \
		--depends iptables \
		--depends sudo \
		$(RPM_COMPRESSION) \
		$(AMBIENT_FILES)

# Package the sidecar deb file.
ambient_deb/fpm:
	rm -f ${TARGET_OUT_LINUX}/release/istio-ambient.deb
	fpm -s dir -t deb -n ${AMBIENT_PACKAGE_NAME} -p ${TARGET_OUT_LINUX}/release/istio-ambient.deb --version $(PACKAGE_VERSION) -f \
		--url http://istio.io  \
		--license Apache \
		--vendor istio.io \
		--architecture "${TARGET_ARCH}" \
		--maintainer istio@istio.io \
		--after-install tools/packaging/postinst-ambient.sh \
		--description "Istio Ambient" \
		--depends iproute2 \
		--depends iptables \
		--depends sudo \
		--conflicts istio-sidecar \
		$(DEB_COMPRESSION) \
		$(AMBIENT_FILES)

.PHONY: \
	ambient_deb \
	ambient_deb/fpm \
	ambient_rpm/fpm
