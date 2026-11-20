#!/usr/bin/env bash
# End-to-end test against a real Docker daemon.
#
#   docker build -t container-redeploy-webhook:dev . && scripts/e2e.sh
#
# Env: IMAGE (default container-redeploy-webhook:dev), DOCKER_SOCK (default: derived from the docker
# context, e.g. /var/run/docker.sock), PORT (default 18080).
#
# Everything it creates is prefixed "redeploy-e2e" and removed at the end.
# To simulate a new image version it temporarily re-tags busybox:1.37.0-uclibc
# to an older busybox locally; the webhook's pull restores the real tag.
set -euo pipefail

IMAGE=${IMAGE:-container-redeploy-webhook:dev}
PORT=${PORT:-18080}
SOCK=${DOCKER_SOCK:-$(docker context inspect --format '{{.Endpoints.docker.Host}}' | sed 's#^unix://##')}
P=redeploy-e2e
ID=redeploy-e2e-demo
TOKEN=e2e-token-0123456789abcdef
REF=busybox:1.37.0-uclibc
OLD=busybox:1.36.1-uclibc
URL=http://127.0.0.1:$PORT

pass=0
fail=0
ok() { echo "  ok   $*"; pass=$((pass + 1)); }
ko() { echo "  FAIL $*"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else ko "$1"; fi; }

cleanup() {
  docker rm -f $(docker ps -aq --filter "name=^$P") >/dev/null 2>&1 || true
  docker network rm $P-net $P-net2 $P-proxynet >/dev/null 2>&1 || true
  docker volume rm $P-vol >/dev/null 2>&1 || true
  docker volume rm $(docker volume ls -q --filter "label=$P") >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

# call METHOD PATH [TOKEN] -> sets $code and $body
call() {
  local out
  out=$(curl -s -w '\n%{http_code}' -X "$1" ${3:+-H "Authorization: Bearer $3"} "$URL$2")
  code=${out##*$'\n'}
  body=${out%$'\n'*}
}

wait_healthy() {
  for _ in $(seq 1 50); do
    curl -fs "$URL/healthz" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "webhook did not come up"; docker logs "$1"; return 1
}

echo "== preparing"
docker pull -q $OLD >/dev/null
docker pull -q $REF >/dev/null
NEW_ID=$(docker image inspect -f '{{.Id}}' $REF)
OLD_ID=$(docker image inspect -f '{{.Id}}' $OLD)
docker tag $OLD $REF   # pretend the local copy of REF is outdated

docker network create $P-net >/dev/null
docker network create $P-net2 >/dev/null
docker volume create $P-vol >/dev/null

docker run -d --name $P-app \
  --label skvostik.redeploy.id=$ID \
  --network $P-net --network-alias web \
  -v $P-vol:/data -v /anon \
  -e FOO=bar \
  -p 127.0.0.1:18081:8080 \
  --restart unless-stopped \
  $REF httpd -f -p 8080 >/dev/null
docker network connect --alias web2 $P-net2 $P-app
docker exec $P-app sh -c 'echo named > /data/marker; echo anon > /anon/marker'
APP_ID=$(docker inspect -f '{{.Id}}' $P-app)

start_hook() { # extra docker run args...
  docker rm -f $P-hook >/dev/null 2>&1 || true
  docker run -d --name $P-hook -p 127.0.0.1:$PORT:8080 "$@" $IMAGE >/dev/null
  wait_healthy $P-hook
}

echo "== direct socket"
start_hook -v "$SOCK:/var/run/docker.sock" -e TOKEN=$TOKEN -e LOG_LEVEL=debug

call POST /redeploy/$ID;              check "no token -> 401" '[ "$code" = 401 ]'
call POST /redeploy/$ID wrong-token;  check "wrong token -> 401" '[ "$code" = 401 ]'
call POST /redeploy/nope $TOKEN;      check "unknown id -> 401" '[ "$code" = 401 ]'

call POST /redeploy/$ID $TOKEN
check "redeploy -> 200 ($body)" '[ "$code" = 200 ]'
check "status updated" 'echo "$body" | grep -q "\"status\":\"updated\""'
NEW_APP_ID=$(docker inspect -f '{{.Id}}' $P-app)
check "container replaced" '[ "$NEW_APP_ID" != "$APP_ID" ]'
check "runs new image" '[ "$(docker inspect -f "{{.Image}}" $P-app)" = "$NEW_ID" ]'
check "running" '[ "$(docker inspect -f "{{.State.Status}}" $P-app)" = running ]'
check "env kept" 'docker inspect -f "{{json .Config.Env}}" $P-app | grep -q FOO=bar'
check "restart policy kept" '[ "$(docker inspect -f "{{.HostConfig.RestartPolicy.Name}}" $P-app)" = unless-stopped ]'
check "port kept" 'curl -fs -o /dev/null http://127.0.0.1:18081/ || [ "$(docker port $P-app 8080/tcp)" = 127.0.0.1:18081 ]'
check "alias on net" 'docker inspect -f "{{json .NetworkSettings.Networks}}" $P-app | grep -q "\"web\""'
check "alias on net2" 'docker inspect -f "{{json .NetworkSettings.Networks}}" $P-app | grep -q "\"web2\""'
check "named volume data" '[ "$(docker exec $P-app cat /data/marker)" = named ]'
check "anonymous volume data" '[ "$(docker exec $P-app cat /anon/marker)" = anon ]'
check "old container removed" '[ -z "$(docker ps -aq --filter name=^$P-app-old)" ]'

call POST /redeploy/$ID $TOKEN
check "second call -> up-to-date" '[ "$code" = 200 ] && echo "$body" | grep -q "up-to-date"'
check "not recreated" '[ "$(docker inspect -f "{{.Id}}" $P-app)" = "$NEW_APP_ID" ]'

call POST "/redeploy/$ID?force=1" $TOKEN
check "force -> updated" '[ "$code" = 200 ] && echo "$body" | grep -q "\"status\":\"updated\""'

docker stop $P-app >/dev/null
call POST /redeploy/$ID $TOKEN
check "stopped container -> 409" '[ "$code" = 409 ]'
check "stopped container untouched" '[ "$(docker inspect -f "{{.State.Status}}" $P-app)" = exited ]'
docker start $P-app >/dev/null

echo "== self protection"
start_hook -v "$SOCK:/var/run/docker.sock" -e TOKEN=$TOKEN --label skvostik.redeploy.id=$P-self
call POST /redeploy/$P-self $TOKEN
check "own container -> 409" '[ "$code" = 409 ]'

echo "== TOKEN unset, no github label"
start_hook -v "$SOCK:/var/run/docker.sock"
call POST /redeploy/$ID $TOKEN
check "no auth method -> 401" '[ "$code" = 401 ]'

echo "== through docker-socket-proxy"
docker network create $P-proxynet >/dev/null
docker pull -q tecnativa/docker-socket-proxy:latest >/dev/null
docker run -d --name $P-proxy --network $P-proxynet --privileged \
  -v "$SOCK:/var/run/docker.sock:ro" \
  -e CONTAINERS=1 -e IMAGES=1 -e NETWORKS=1 -e POST=1 \
  tecnativa/docker-socket-proxy:latest >/dev/null
start_hook --network $P-proxynet -e DOCKER_HOST=tcp://$P-proxy:2375 -e TOKEN=$TOKEN
BEFORE=$(docker inspect -f '{{.Id}}' $P-app)
call POST "/redeploy/$ID?force=1" $TOKEN
check "via proxy -> 200 ($body)" '[ "$code" = 200 ] && echo "$body" | grep -q "\"status\":\"updated\""'
check "via proxy: replaced" '[ "$(docker inspect -f "{{.Id}}" $P-app)" != "$BEFORE" ]'

echo
echo "passed: $pass, failed: $fail"
if [ "$fail" -gt 0 ]; then
  echo "--- webhook logs"; docker logs $P-hook 2>&1 | tail -30
  exit 1
fi
