#!/bin/bash
# PostgreSQL's pg_hba.conf treats the "replication" pseudo-database as
# distinct from the "all" keyword — a generic `host all all all <method>`
# rule does NOT grant replication connections. The official postgres image's
# entrypoint only auto-generates a replication rule for loopback addresses
# (127.0.0.1/::1), which does not cover connections arriving from outside
# the container via Docker's published port. Without this script,
# internal/shadowflow's replication-mode connections (decoder.go,
# replication.go) would fail with "no pg_hba.conf entry for replication
# connection" when run against this dev container.
#
# This script runs as part of docker-entrypoint-initdb.d, after the
# entrypoint has already finalized pg_hba.conf but before the server starts
# for real — appending here is safe and takes effect on the next start.
set -e
echo "host replication all all trust" >> "$PGDATA/pg_hba.conf"
