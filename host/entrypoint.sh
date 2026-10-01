#!/bin/sh
# Starts the reference overlay host with the borg module. Two things are
# added to the stock start:
#
#  - BORG_HALLS is required, and when OVERLAY_TOPICS is unset it is derived
#    from it (tm_borg_<hall> for each entry), so a host is configured by one
#    variable: the line `borg hall new` prints. The module checks the
#    grammar.
#  - an unset OVERLAY_ADMIN_TOKEN is replaced by a random one for this run,
#    because the host requires one. Nothing can use the admin surface until
#    the operator sets their own.
set -eu

if [ -z "${BORG_HALLS:-}" ]; then
  echo "borg-host: BORG_HALLS is empty. Create a hall with 'borg hall new <name>' and set BORG_HALLS to the hall it prints (several halls: comma separated)." >&2
  exit 64
fi

if [ -z "${OVERLAY_TOPICS:-}" ]; then
  topics=""
  for hall in $(printf '%s' "$BORG_HALLS" | tr ',' ' '); do
    topics="${topics:+$topics,}tm_borg_$hall"
  done
  OVERLAY_TOPICS=$topics
  export OVERLAY_TOPICS
fi

if [ -z "${OVERLAY_ADMIN_TOKEN:-}" ]; then
  OVERLAY_ADMIN_TOKEN=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
  export OVERLAY_ADMIN_TOKEN
  echo "borg-host: OVERLAY_ADMIN_TOKEN is unset; using a random one for this run (set it to use /admin)" >&2
fi

echo "borg-host: topics $OVERLAY_TOPICS" >&2
exec node /app/dist/index.js "$@"
