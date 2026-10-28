# Mailpit catch-all SMTP server for the local MiniCRM test stack.
#
# The upstream image is used as-is; only a wait-for-certificate entrypoint wrapper is
# added so the container survives starting before the one-shot "certgen" service has
# written /certs/tls.crt (see entrypoint.sh).
FROM axllent/mailpit:v1.29

COPY entrypoint.sh /usr/local/bin/mailpit-entrypoint
RUN chmod +x /usr/local/bin/mailpit-entrypoint

ENTRYPOINT ["/usr/local/bin/mailpit-entrypoint"]
