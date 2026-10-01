#!/usr/bin/env bash
# QUICKSTART.md, run as written, in a throwaway copy of the quickstart
# compose project: the local chain, two hosts and MySQL; an organisation,
# three members and host-a's payee; a hall; the organisation created in
# unicast mode with three grants; a group whose epoch key every member
# opens; a removal that rotates the epoch, which the removed member cannot
# open; the roll read and compared at both hosts; a grant revoked by sweep;
# the priced history question paid, and the payee settling it; the removal
# the revoked member is owed, made; a fill that finds the two hosts agree.
#
#   scripts/quickstart-check.sh
#
# Needs the three images the compose file names (make docker-build, make
# docker-build-host, or the published ones). COMPOSE names the compose
# command (default "docker compose", else Compose from the docker:cli
# image with the work directory mounted where it is). Everything it
# creates, volumes included, is removed at the end, pass or fail.
set -euo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
project=borg-quickstart-check-$$
cp "$here/deploy/quickstart/compose.yaml" "$work/"
cd "$work"
if [ -n "${COMPOSE:-}" ]; then
	compose=$COMPOSE
elif docker compose version >/dev/null 2>&1; then
	compose="docker compose"
else
	compose="docker run --rm -i -v /var/run/docker.sock:/var/run/docker.sock -v $work:$work -w $work docker:cli compose"
fi

dc() { $compose -p "$project" "$@"; }
as() { local who=$1; shift; dc run --rm -T -e "BORG_HOME=/home/nonroot/.borg/$who" borg "$@"; }
cleanup() {
	dc --profile cli down -v --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT
step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "quickstart-check: $*" >&2
	dc logs --no-color --tail 40 >&2 || true
	exit 1
}
key() { sed -n 's/^identity *//p' "$1"; }

step "1. the local chain"
dc up -d chain

step "2. an organisation, three members, a hall, the payee key"
as org init | tee org.txt
as org fund
for m in alice bob carol; do as $m init | tee $m.txt; done
as bob fund
ORG=$(key org.txt) ALICE=$(key alice.txt) BOB=$(key bob.txt) CAROL=$(key carol.txt)
[ -n "$ORG" ] && [ -n "$ALICE" ] && [ -n "$BOB" ] && [ -n "$CAROL" ] || fail "init printed no identity"
as payee init
as org hall new example | tee hall.txt
sed -n 's/^host *//p' hall.txt >.env
echo "BORG_ORG=$ORG" >>.env
as payee payee key -out - >>.env
grep -q '^BORG_HALLS=example_[a-z]\{10\}$' .env && grep -q '^BORG_PAYEE_KEY=[0-9a-f]\{64\}$' .env || fail "the .env lines are missing"

step "3. two hosts that carry the hall"
dc up -d
for i in $(seq 1 90); do
	out=$(as org doctor 2>/dev/null || true)
	[ "$(printf '%s\n' "$out" | grep -c '^host .* answering')" = 2 ] && printf '%s\n' "$out" | grep -q '^history .* serves terms' && break
	sleep 2
done
printf '%s\n' "$out"
[ "$(printf '%s\n' "$out" | grep -c '^host .* answering')" = 2 ] || fail "the hosts did not come up"

step "4. the organisation and a group"
as org org create 'Example Organisation' -member "$ALICE" -member "$BOB" -member "$CAROL" | tee create.txt
[ "$(grep -c '^grant ' create.txt)" = 3 ] || fail "org create did not issue three grants"
grep -q '^host http://host-b:8080: took .* missed 0' create.txt || fail "host-b missed something"
as org group create engineering -member "$ALICE" -member "$BOB" -member "$CAROL" | tee group.txt
GROUP=$(sed -n 's/^group *//p' group.txt)
[ -n "$GROUP" ] || fail "group create printed no group"

step "5. every member opens the epoch key"
for m in alice bob carol; do as $m open engineering | tee open-$m.txt; grep -q '^epoch   0 OPENED' open-$m.txt || fail "$m did not open epoch 0"; done

step "6. remove bob: a new epoch bob cannot open"
as org org remove "$BOB" -group engineering | tee remove.txt
grep -q 'epoch 1 (a new epoch' remove.txt || fail "the removal did not rotate"
as alice open engineering | tee open2.txt
grep -q '^epoch   1 OPENED' open2.txt || fail "alice did not open epoch 1"
rc=0; as bob open engineering >bob2.txt 2>&1 || rc=$?
cat bob2.txt
[ "$rc" = 1 ] && grep -q 'epoch   1 CANNOT OPEN' bob2.txt || fail "bob opened epoch 1, or exited $rc"

step "7. the roll at both hosts, and a revocation by sweep"
as alice roll 2>&1 | tee roll.txt
[ "$(grep -c '^host .* the root of seq [0-9]* reproduced' roll.txt)" = 2 ] || fail "the hosts' rolls do not both reproduce the root"
as org grant revoke "$CAROL" -by sweep | tee revoke.txt
grep -q '^sweep ' revoke.txt || fail "revoke made no sweep"
rc=0; as carol member "$CAROL" >carol.txt 2>&1 || rc=$?
cat carol.txt
[ "$rc" = 1 ] && grep -q "not current, revoked" carol.txt || fail "carol is still a current member"

step "8. the priced question, and the payee settling it"
as bob terms | tee terms.txt
grep -q '^history        5 sat a question' terms.txt || fail "terms"
as bob history -chain "$GROUP" | tee history.txt
grep -q '^paid 5 sat to ' history.txt && grep -q '^seq 1 ' history.txt || fail "history"
as payee payee settle /var/lib/borg-a/payments.jsonl | tee settle.txt
grep -q '^1 payment(s) settled, 5 sat' settle.txt || fail "payee settle"

step "9. the removal owed, and the hosts agree"
rc=0; as org org due >due.txt 2>&1 || rc=$?
cat due.txt
[ "$rc" = 3 ] && grep -q "^due     group $GROUP engineering: $CAROL" due.txt || fail "org due did not list carol, or exited $rc"
as org org due -apply | tee apply.txt
grep -q "^removed $CAROL: rotation debt" apply.txt && grep -q '^applied 1 group(s)' apply.txt || fail "org due -apply did not remove carol"
as alice fill | tee fill.txt
grep -q '^filled  0 object(s) at 0 host(s)' fill.txt || fail "fill found the hosts differ"

echo
echo "quickstart-check: ok"
