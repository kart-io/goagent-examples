#!/usr/bin/env bash
# 启动 Postgres(pgvector) 与 Qdrant 专用容器，导出 CS_MEMORY_PG_URL、CS_MEMORY_QDRANT_ADDR 后执行给定命令，结束后删除容器。
# 另导出 CS_MEMORY_PG_CONTAINER、CS_MEMORY_QDRANT_CONTAINER，故障测试只暂停或删除这两个容器。
# 用法：scripts/with-stores.sh <命令> [参数...]，如 scripts/with-stores.sh go test ./orchestration/customer-service/
# 端口可用 CS_MEMORY_PG_PORT、CS_MEMORY_QDRANT_PORT、CS_MEMORY_QDRANT_HTTP_PORT 覆盖。
set -euo pipefail

[ $# -gt 0 ] || { echo "用法：$0 <命令> [参数...]" >&2; exit 2; }

pg=cs-mem-pg-$$
qdrant=cs-mem-qdrant-$$
pg_port=${CS_MEMORY_PG_PORT:-55450}
qdrant_port=${CS_MEMORY_QDRANT_PORT:-56344}
qdrant_http_port=${CS_MEMORY_QDRANT_HTTP_PORT:-56343}

trap 'docker rm -f "$pg" "$qdrant" >/dev/null 2>&1 || true' EXIT

docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=test -p "127.0.0.1:$pg_port:5432" pgvector/pgvector:pg17 >/dev/null
# 存储放 tmpfs：机械盘上建集合要 9～14 秒
docker run -d --rm --name "$qdrant" --tmpfs /qdrant/storage -p "127.0.0.1:$qdrant_port:6334" -p "127.0.0.1:$qdrant_http_port:6333" qdrant/qdrant:v1.20.2 >/dev/null

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
wait_for Postgres docker exec "$pg" psql -U postgres -qc 'SELECT 1'
wait_for Qdrant curl -fsS "http://127.0.0.1:$qdrant_http_port/readyz"

export CS_MEMORY_PG_URL="postgres://postgres:test@127.0.0.1:$pg_port/postgres?sslmode=disable"
export CS_MEMORY_QDRANT_ADDR="127.0.0.1:$qdrant_port"
export CS_MEMORY_PG_CONTAINER="$pg"
export CS_MEMORY_QDRANT_CONTAINER="$qdrant"

"$@"
