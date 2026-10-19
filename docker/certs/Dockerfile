# One-shot certificate generator for the local Dovecot IMAP server.
FROM alpine:3.21

RUN apk add --no-cache openssl

COPY generate-certs.sh /usr/local/bin/generate-certs.sh
RUN chmod +x /usr/local/bin/generate-certs.sh

ENTRYPOINT ["/usr/local/bin/generate-certs.sh"]
