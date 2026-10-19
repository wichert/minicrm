# Minimal Dovecot IMAP server used by the local MiniCRM test stack.
#
# It serves two static accounts (support@example.com and sales@example.com, password
# "secret"), accepts IMAP over implicit TLS (993) and STARTTLS (143), and seeds a few
# sample messages into each account's INBOX on first start (see entrypoint.sh).
FROM alpine:3.21

RUN apk add --no-cache dovecot ca-certificates tzdata \
 && addgroup -S -g 5000 vmail \
 && adduser -S -D -H -u 5000 -G vmail vmail

COPY dovecot.conf /etc/dovecot/dovecot.conf
COPY users /etc/dovecot/users
COPY entrypoint.sh /usr/local/bin/dovecot-entrypoint
COPY seed/ /seed/

RUN chmod +x /usr/local/bin/dovecot-entrypoint \
 && mkdir -p /srv/vmail /run/dovecot

EXPOSE 143 993

HEALTHCHECK --interval=3s --timeout=3s --start-period=5s --retries=20 \
    CMD nc -z 127.0.0.1 993 || exit 1

ENTRYPOINT ["/usr/local/bin/dovecot-entrypoint"]
