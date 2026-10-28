#!/bin/sh
set -eu
umask 077
if [ "${1:-}" = initialize ]; then
    mkdir -p /client /server
    if [ ! -f /client/id_ed25519 ]; then
        ssh-keygen -q -t ed25519 -N '' -f /client/id_ed25519
    fi
    if [ ! -f /server/ssh_host_ed25519_key ]; then
        ssh-keygen -q -t ed25519 -N '' -f /server/ssh_host_ed25519_key
    fi
    ssh-keygen -y -f /client/id_ed25519 > /server/authorized_keys
    chmod 0755 /server
    chmod 0600 /server/ssh_host_ed25519_key /client/id_ed25519
    chmod 0644 /server/authorized_keys
    chown -R 10001:10001 /client
    chmod 0700 /client
    exit 0
fi
mkdir -p /run/sshd /workspace /home/albert
chown albert:albert /workspace /home/albert
exec /usr/sbin/sshd -D -e
