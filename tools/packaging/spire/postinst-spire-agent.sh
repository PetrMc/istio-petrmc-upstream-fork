#!/bin/bash
#
# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant
#
################################################################################

set -e

umask 022

if ! getent passwd istio-proxy >/dev/null; then
    if command -v useradd >/dev/null; then
        groupadd --system istio-proxy
        useradd --system --gid istio-proxy --home-dir /var/lib/spire istio-proxy
    else
        addgroup --system istio-proxy
        adduser --system --group --home /var/lib/spire istio-proxy
    fi
fi

if [ ! -e /etc/spire ]; then
   # Backward compat.
   ln -s /var/lib/spire /etc/spire
fi

mkdir -p /var/lib/spire/certs
mkdir -p /var/run/spire/
mkdir -p /var/log/spire

chown -R istio-proxy.istio-proxy /var/lib/spire/ /var/run/spire/ /var/log/spire/
chmod o+rx /usr/local/bin/spire-agent
chmod 2755 /usr/local/bin/spire-agent
