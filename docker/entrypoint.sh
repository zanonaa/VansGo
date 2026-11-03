#!/bin/sh
set -eu

chown -R app:app /app/data 2>/dev/null || true

# No locality knob here on purpose. Docker NATs published-port traffic from the
# host, so the app sees the container gateway as the TCP peer and cannot tell the
# host from the LAN (on Docker Desktop / rootless the LAN is masqueraded to the
# same address). Instead of guessing, a non-local login on the default password
# gets a password-change-only grant from the API — see /api/auth/login.

# A migração do volume legado roda dentro do binário no boot.

# $HOME do processo aponta para o DATA_DIR: o proxy headroom (Python,
# spawnado pelo supervisor Go) grava cache/config em $HOME e, no runner
# padrão da imagem, /root não é gravável pelo usuário app.
export HOME="${DATA_DIR:-/app/data}"

exec su-exec app "$@"
