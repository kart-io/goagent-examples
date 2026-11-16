#!/usr/bin/env bash
# 真实模型验收：用真实对话模型与 Gemini 嵌入跑规格第 6 节场景 1–3，报告写入 verification/live-<UTC时间>.json。
# 会调用付费 API，须显式 KA_LIVE=1 开启。密钥从 shell 环境或 .env 读取，报告里不含密钥。
# 用法：KA_LIVE=1 scripts/live.sh
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "${KA_LIVE:-}" != 1 ]; then
	echo "未设置 KA_LIVE=1，跳过真实模型验收（会调用付费模型 API）"
	exit 0
fi

# 用 compose 解析后的配置判断，.env 与 shell 环境都算在内；脚本模型或哈希嵌入跑出的结果不算真实验收。
config=$(docker compose config --format json)
model=$(jq -r '.services.a.environment.KA_MODEL' <<<"$config")
embedder=$(jq -r '.services.a.environment.KA_EMBEDDER' <<<"$config")
provider=$(jq -r '.services.a.environment.GOAGENT_LIVE_PROVIDER' <<<"$config")
unset config
if [ "$model" != live ] || [ "$embedder" != gemini ]; then
	echo "真实模型验收要求 KA_MODEL=live、KA_EMBEDDER=gemini，当前为 $model/$embedder" >&2
	exit 1
fi

docker compose up -d --build
base_a="http://127.0.0.1:$(docker compose port a 8081 | sed 's/.*://')"
base_b="http://127.0.0.1:$(docker compose port b 8081 | sed 's/.*://')"
for base in "$base_a" "$base_b"; do
	for i in $(seq 120); do
		curl -fsS "$base/" >/dev/null 2>&1 && break
		[ "$i" = 120 ] && { echo "$base 60 秒内未就绪" >&2; exit 1; }
		sleep 0.5
	done
done

# date +%N 是 GNU 扩展，macOS 不支持；perl 在两边都自带。
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time() * 1000'; }

# uuidgen 在 macOS 与多数 Linux 上都有；精简的 Linux 镜像没有时退回内核接口。
new_uuid() { uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid; }

# api <base> <user> <method> <path> [body]：输出响应体，非 2xx 时报错退出。
api() {
	curl -fsS -X "$3" "$1$4" -H "Authorization: Bearer $2" \
		-H "Idempotency-Key: $(new_uuid)" -H 'Content-Type: application/json' ${5:+-d "$5"}
}

# ask <base> <user> <question>：新建会话提问，等到终态，输出 {state, answer, tokens, usage_status, elapsed_ms}。
ask() {
	local base=$1 user=$2 question=$3 agent session task state start
	agent=$(api "$base" "$user" POST /api/agent | jq -r .agent_id)
	session=$(api "$base" "$user" POST /v1/sessions "$(jq -nc --arg a "$agent" '{agent_id:$a}')" | jq -r .id)
	for _ in $(seq 120); do
		[ "$(api "$base" "$user" GET "/v1/sessions/$session" | jq -r .status)" = ready ] && break
		sleep 0.5
	done
	start=$(now_ms)
	task=$(api "$base" "$user" POST /v1/tasks "$(jq -nc --arg s "$session" --arg q "$question" '{session_id:$s,input:$q}')" | jq -r .id)
	local body
	for _ in $(seq 360); do
		body=$(api "$base" "$user" GET "/v1/tasks/$task")
		state=$(jq -r .state <<<"$body")
		case $state in succeeded | failed | canceled | attention | waiting_approval | waiting_input) break ;; esac
		sleep 0.5
	done
	local elapsed=$(($(now_ms) - start))
	local run
	run=$(api "$base" "$user" GET "/v1/runs/$task")
	jq -nc --argjson task "$body" --argjson run "$run" --argjson ms "$elapsed" '{
		task_id: $task.id, state: $task.state,
		answer: ($task.result | if type == "string" then . else tojson end),
		tokens: $run.usage.Root.KnownTokens, usage_status: $run.usage_status, elapsed_ms: $ms}'
}

# memory_ids <base> <user> <q> <text>：输出置顶或召回结果中正文含 text 的记忆 ID（每行一个）。
memory_ids() {
	api "$1" "$2" GET "/api/memories?q=$(jq -rn --arg q "$3" '$q|@uri')" |
		jq -r --arg t "$4" '(.pinned + .matches)[] | select(.content | contains($t)) | .id' | sort -u
}

cache_keys() { docker compose exec -T redis redis-cli --scan --pattern 'ka:cache:e:*' | sort; }

# sources_of 只取带 #小节 的写法，并在中英文标点、括号处截断，避免把正文里的 goagent:cache: 之类字面量当成来源。
sources_of() { grep -oE 'goagent(-contrib)?:[A-Za-z0-9_./-]+#[^[:space:]，。、；：（）()`"*]+' <<<"$1" | sort -u | jq -Rsc 'split("\n") | map(select(. != ""))'; }

# retrieved_for <base> <user> <question>：同一问题在索引里实际检索到的来源。
retrieved_for() { api "$1" "$2" GET "/api/sources?q=$(jq -rn --arg q "$3" '$q|@uri')" | jq -c '[.[].source] | unique'; }

results=()
failures=0
fail() {
	echo "失败：$*" >&2
	failures=$((failures + 1))
}
record() { results+=("$1"); }

run_id=$(date -u +%Y%m%dT%H%M%SZ)
tag=$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')

# 场景 1：文档问答必须引用期望仓库路径。
docs_user=live-docs-$tag
while IFS='|' read -r question prefix inst; do
	base=$base_a
	[ "$inst" = b ] && base=$base_b
	echo "== 文档问答（$inst）：$question"
	r=$(ask "$base" "$docs_user" "$question")
	answer=$(jq -r .answer <<<"$r")
	sources=$(sources_of "$answer")
	retrieved=$(retrieved_for "$base" "$docs_user" "$question")
	# 只认回答里原样出现的检索来源（小节名可含空格，故按子串而非正则结果比对），编造的路径不会命中。
	# 只有包文档的目录来源形如 repo:dir#godoc，没有结尾斜杠，所以目录前缀也接受 dir#。
	cited=$(jq -c --arg a "$answer" '[.[] | select(. as $s | $a | contains($s))]' <<<"$retrieved")
	ok=$(jq -n --argjson s "$cited" --arg p "$prefix" '[$s[] | select(startswith($p) or startswith(($p | rtrimstr("/")) + "#"))] | length > 0')
	[ "$(jq -r .state <<<"$r")" = succeeded ] || fail "「$question」任务状态 $(jq -r .state <<<"$r")"
	[ "$ok" = true ] || fail "「$question」回答引用的检索来源中没有以 $prefix 开头的：引用 $cited，检索 $retrieved"
	record "$(jq -c --arg q "$question" --arg p "$prefix" --arg i "$inst" --argjson s "$sources" --argjson rt "$retrieved" \
		--argjson c "$cited" --argjson ok "$ok" \
		'{scenario:"rag", instance:$i, question:$q, expected_prefix:$p, sources_in_answer:$s, retrieved_sources:$rt,
		  cited_retrieved:$c, recalled:null, pass:$ok} + .' <<<"$r")"
done <<'EOF'
pgstore 怎么建表|goagent-contrib:durable/pgstore/|a
模型缓存怎么在删除记忆后擦除|goagent:docs/|b
Redis 缓存前缀默认是什么|goagent-contrib:cache/redis/|a
EOF

# 场景 2–3：a 上写入记忆，b 上新会话召回，经 b 删除后 a 不再召回，召回时产生的模型缓存条目随之物理删除。
# 事实用文档里不可能出现的代号，回答中出现它只能来自记忆。
mem_user=live-mem-$tag
code_name="orion-$tag"
fact="我们团队的值班集群代号是 $code_name"
probe="我们团队的值班集群代号是什么？"

echo "== 记忆写入（a）：$fact"
r=$(ask "$base_a" "$mem_user" "记住：$fact")
[ "$(jq -r .state <<<"$r")" = succeeded ] || fail "写入记忆任务状态 $(jq -r .state <<<"$r")"
ids=""
for _ in $(seq 120); do
	ids=$(memory_ids "$base_b" "$mem_user" "值班集群代号" "$code_name")
	[ -n "$ids" ] && break
	sleep 1
done
[ -n "$ids" ] || fail "经 b 的 /api/memories 查不到 a 写入的记忆"
record "$(jq -c --arg q "记住：$fact" --argjson n "$(grep -c . <<<"$ids" || true)" \
	'{scenario:"remember", instance:"a", question:$q, expected_prefix:null, sources_in_answer:[], recalled:($n > 0), records_visible_on_b:$n} + .' <<<"$r")"

echo "== 跨实例召回（b，新会话）：$probe"
before=$(cache_keys)
r=$(ask "$base_b" "$mem_user" "$probe")
after=$(cache_keys)
added=$(comm -13 <(echo "$before") <(echo "$after") | grep . || true)
recalled=$(jq --arg c "$code_name" '.answer | contains($c)' <<<"$r")
[ "$recalled" = true ] || fail "b 上新会话的回答没有召回记忆：$(jq -r .answer <<<"$r")"
[ -n "$added" ] || fail "召回提问后 Redis 中没有新增模型缓存条目（ka:cache:e:*），模型缓存未生效"
record "$(jq -c --arg q "$probe" --argjson rc "$recalled" --argjson n "$(grep -c . <<<"$added" || true)" \
	'{scenario:"recall", instance:"b", question:$q, expected_prefix:null, sources_in_answer:[], recalled:$rc, cache_entries_added:$n} + .' <<<"$r")"

echo "== 经 b 删除记忆"
ids=$(memory_ids "$base_b" "$mem_user" "值班集群代号" "$code_name")
for id in $ids; do
	api "$base_b" "$mem_user" DELETE "/api/memories/$(jq -rn --arg q "$id" '$q|@uri')" >/dev/null
done
# 抽取是异步的：等一会儿再查，确认删除前登记的抽取任务没有把这条事实重新写回。
sleep 20
left_a=$(memory_ids "$base_a" "$mem_user" "值班集群代号" "$code_name")
[ -z "$left_a" ] || fail "经 b 删除后 a 仍能查到记忆：$left_a"
remaining=0
for k in $added; do
	remaining=$((remaining + $(docker compose exec -T redis redis-cli exists "$k")))
done
[ "$remaining" = 0 ] || fail "删除记忆后仍有 $remaining 个召回时产生的缓存条目存在"

echo "== 删除后召回（a，新会话）：$probe"
r=$(ask "$base_a" "$mem_user" "$probe")
recalled=$(jq --arg c "$code_name" '.answer | contains($c)' <<<"$r")
[ "$recalled" = false ] || fail "删除后 a 的回答仍含记忆：$(jq -r .answer <<<"$r")"
record "$(jq -c --arg q "$probe" --argjson rc "$recalled" --argjson del "$(grep -c . <<<"$ids" || true)" \
	--argjson left "$(grep -c . <<<"$left_a" || true)" --argjson rem "$remaining" \
	'{scenario:"after_forget", instance:"a", question:$q, expected_prefix:null, sources_in_answer:[], recalled:$rc,
	  records_deleted_via_b:$del, records_left_on_a:$left, cache_entries_left:$rem} + .' <<<"$r")"

mkdir -p verification
report=verification/live-$run_id.json
printf '%s\n' "${results[@]}" | jq -s --arg at "$run_id" --arg p "$provider" --arg e "$embedder" \
	--arg commit "$(git describe --always --dirty 2>/dev/null || echo unknown)" --argjson f "$failures" \
	'{run_at:$at, commit:$commit, provider:$p, embedder:$e, failures:$f, passed:($f == 0), questions:.}' >"$report"
echo "报告：$report"
if [ "$failures" -gt 0 ]; then
	echo "真实模型验收失败：$failures 项" >&2
	exit 1
fi
echo "真实模型验收通过"
