#!/bin/sh
# Generates, into the directory given as the first argument, what the
# integration PostgreSQL needs: ca.crt, server.crt and server.key (a throwaway
# CA and a server certificate for postgres, localhost and 127.0.0.1) and
# admin-password (random). POSIX sh and openssl only, so it runs on a CI
# runner and in the alpine/openssl image of the devcontainer stack alike.
# Everything is valid for two days and meant to be thrown away.
set -eu

dir="${1:?usage: gen-certs.sh <directory>}"
mkdir -p "$dir"
cd "$dir"
umask 077

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
    -keyout ca.key -out ca.crt -subj "/CN=operator-db integration CA" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign"
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout server.key -out server.csr -subj "/CN=postgres"
cat > server.ext <<'EXT'
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:postgres,DNS:localhost,IP:127.0.0.1
EXT
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -days 2 -extfile server.ext -out server.crt
openssl rand -hex 16 > admin-password
rm -f ca.key ca.srl server.csr server.ext

# The key stays 0600; the rest is read by the test process, which may be
# another user (the devcontainer's).
chmod 755 .
chmod 644 ca.crt server.crt admin-password
