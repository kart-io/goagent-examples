#!/usr/bin/env bash
# 启动 Postgres(pgvector)、Redis、Qdrant 专用容器并运行产品全部测试，结束后删除容器。
# 用法：scripts/test.sh [go test 参数]，默认包范围为 ./...
set -euo pipefail
cd "$(dirname "$0")/.."

pg=ka-test-pg-$$
redis=ka-test-redis-$$
qdrant=ka-test-qdrant-$$
pg_port=${KA_TEST_PG_PORT:-55440}
redis_port=${KA_TEST_REDIS_PORT:-56390}
qdrant_port=${KA_TEST_QDRANT_PORT:-56334}
qdrant_http_port=${KA_TEST_QDRANT_HTTP_PORT:-56333}

trap 'docker rm -f "$pg" "$redis" "$qdrant" >/dev/null 2>&1 || true' EXIT

docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=test -p "127.0.0.1:$pg_port:5432" pgvector/pgvector:pg17 >/dev/null
docker run -d --rm --name "$redis" -p "127.0.0.1:$redis_port:6379" redis:7-alpine >/dev/null
docker run -d --rm --name "$qdrant" -p "127.0.0.1:$qdrant_port:6334" -p "127.0.0.1:$qdrant_http_port:6333" qdrant/qdrant:v1.20.2 >/dev/null

# 最多等待约 60 秒，超时按服务名报错退出
wait_for() {
	local svc=$1
	shift
	for _ in $(seq 300); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	echo "等待 $svc 就绪超时（60 秒）" >&2
	exit 1
}

wait_for Postgres docker exec "$pg" pg_isready -q
# 官方镜像初始化阶段会重启一次服务，等稳定后再连
sleep 1
wait_for "Postgres(vector 扩展)" docker exec "$pg" psql -U postgres -qc 'CREATE EXTENSION IF NOT EXISTS vector'
wait_for Redis docker exec "$redis" sh -c 'redis-cli ping | grep -q PONG'
wait_for Qdrant curl -fsS "http://127.0.0.1:$qdrant_http_port/readyz"

export KA_TEST_PG_URL="postgres://postgres:test@127.0.0.1:$pg_port/postgres?sslmode=disable"
export KA_TEST_REDIS_ADDR="127.0.0.1:$redis_port"
export KA_TEST_QDRANT_ADDR="127.0.0.1:$qdrant_port"

pkgs=("$@")
[ ${#pkgs[@]} -eq 0 ] && pkgs=(./...)
go vet "${pkgs[@]}"
TMPDIR=/tmp taskset -c 2,3,6-27 go test -count=1 "${pkgs[@]}"
