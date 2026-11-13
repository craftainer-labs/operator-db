#!/bin/sh
# Entrypoint of the integration PostgreSQL (the official image, started as
# root): installs the TLS material that gen-certs.sh wrote to /tls with the
# owner and mode PostgreSQL insists on, then starts the image's own entrypoint
# with TLS on and a pg_hba.conf that accepts nothing but TLS with SCRAM.
# The local socket is trusted, inside the container only (initdb and the
# health check use it).
set -eu

src="${PG_TLS_SRC:-/tls}"
dst=/etc/pg-tls
install -d -m 755 "$dst"
install -m 600 -o postgres -g postgres "$src/server.key" "$dst/server.key"
install -m 644 -o postgres -g postgres "$src/server.crt" "$dst/server.crt"
cat > "$dst/pg_hba.conf" <<'HBA'
local all all trust
hostssl all all all scram-sha-256
hostnossl all all all reject
HBA
chown postgres:postgres "$dst/pg_hba.conf"

exec docker-entrypoint.sh postgres \
    -c listen_addresses='*' \
    -c ssl=on \
    -c ssl_cert_file="$dst/server.crt" \
    -c ssl_key_file="$dst/server.key" \
    -c ssl_min_protocol_version=TLSv1.2 \
    -c hba_file="$dst/pg_hba.conf" \
    -c password_encryption=scram-sha-256 \
    -c log_statement=all
