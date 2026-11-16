#!/usr/bin/env bash
# 把 Worker 从实例 a 转移到 b：强停 a、回收它占着的 Worker 槽位、以 WORKER=1 重建 b。
# 用法：scripts/failover.sh <原因>
# 须在执行 docker compose up 时的同一套环境变量（或同一个 .env）下运行，否则 b 会以不同配置重建。
set -euo pipefail
cd "$(dirname "$0")/.."

reason=${1:?用法：scripts/failover.sh <原因>}

# 1. 强停 a。真实故障里 a 可能已经不在；kill 对已停止的容器会报错，这里只关心它最终不在运行。
docker compose kill a >/dev/null 2>&1 || true

# 2. 回收是"确认旧实例已停止"的声明，a 仍在运行时回收会让两个 Worker 同时执行。
# kill 返回时容器状态可能还没更新，给它几秒。
for _ in $(seq 20); do
	[ -z "$(docker compose ps -q --status running a)" ] && break
	sleep 0.5
done
if [ -n "$(docker compose ps -q --status running a)" ]; then
	echo "a 仍在运行，拒绝回收 Worker 槽位" >&2
	exit 1
fi

# 3. 回收槽位。--no-deps：一次性命令不应顺带重跑 migrate/ingest。
docker compose run --rm --no-deps b ka recover-worker -reason "$reason" -confirm-stopped

# 4. 以 WORKER=1 重建 b，由它认领槽位并执行启动恢复。
WORKER=1 docker compose up -d --no-deps b

# 认领槽位途中进程收到 SIGTERM 会泄漏槽位且仍以 0 退出（FINDINGS F19），
# 所以要等 b 真正执行完一个任务、确认已持有槽位，才把控制权交还给调用方。
port=$(docker compose port b 8081 | sed 's/.*://')
base="http://127.0.0.1:$port"
for _ in $(seq 120); do
	curl -fsS "$base/" >/dev/null 2>&1 && break
	sleep 0.5
done
# 先取全文再匹配：pipefail 下 grep -q 提前退出会让 logs 收到 SIGPIPE，造成误报。
logs_b=$(docker compose logs b)
grep -q 'worker=true' <<<"$logs_b" || { echo "b 未以 Worker 身份启动" >&2; printf '%s\n' "$logs_b" >&2; exit 1; }

user=failover-probe
api() {
	curl -fsS -X "$1" "$base$2" -H "Authorization: Bearer $user" -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
		-H 'Content-Type: application/json' ${3:+-d "$3"}
}
agent=$(api POST /api/agent | jq -r .agent_id)
session=$(api POST /v1/sessions "{\"agent_id\":\"$agent\"}" | jq -r .id)
for _ in $(seq 120); do
	[ "$(api GET "/v1/sessions/$session" | jq -r .status)" = ready ] && break
	sleep 0.5
done
task=$(api POST /v1/tasks "{\"session_id\":\"$session\",\"input\":\"你好\"}" | jq -r .id)
for _ in $(seq 240); do
	state=$(api GET "/v1/tasks/$task" | jq -r .state)
	case $state in
	succeeded)
		echo "故障转移完成：b 已持有 Worker 槽位并执行了探测任务 $task"
		exit 0
		;;
	failed | canceled | attention)
		echo "探测任务停在 $state" >&2
		exit 1
		;;
	esac
	sleep 0.5
done
echo "探测任务 $task 超时未完成（最后状态 $state）" >&2
docker compose logs --tail 50 b >&2
exit 1
