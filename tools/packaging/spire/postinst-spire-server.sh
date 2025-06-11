#!/bin/bash
#
# Copyright Solo.io, Inc
#
# Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant
#
################################################################################

set -e

umask 022

if [ ! -e /etc/spire ]; then
   # Backward compat.
   ln -s /var/lib/spire /etc/spire
fi

mkdir -p /var/lib/spire
mkdir -p /var/log/spire

chmod o+rx /usr/local/bin/spire-server
chmod 2755 /usr/local/bin/spire-server
