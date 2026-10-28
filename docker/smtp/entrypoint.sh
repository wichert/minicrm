#!/bin/sh
# Entrypoint wrapper for the Mailpit container.
#
# Mailpit reads MP_SMTP_TLS_CERT/MP_SMTP_TLS_KEY once at startup and exits with a fatal
# "TLS certificate not found or readable" error when they are absent. The certificate is
# produced by the one-shot "certgen" compose service, which runs concurrently with this
# container, so on a clean stack (podman compose down -v) Mailpit would lose that race.
#
# Wait for the certificate files before exec'ing Mailpit, mirroring docker/mail/entrypoint.sh.
set -eu

CERT="${MP_SMTP_TLS_CERT:-/certs/tls.crt}"
KEY="${MP_SMTP_TLS_KEY:-/certs/tls.key}"

tries=0
while [ ! -s "$CERT" ] || [ ! -s "$KEY" ]; do
    tries=$((tries + 1))
    if [ "$tries" -gt 60 ]; then
        echo "timed out waiting for $CERT and $KEY" >&2
        exit 1
    fi
    sleep 1
done

exec /mailpit "$@"
