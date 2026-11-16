# 构建上下文是 products/knowledge-assistant；只复制编译需要的目录，免得本地 .env 进入构建层。

FROM golang:1.27 AS build
# 新推送的 contrib tag 可能尚未被默认代理收录，回退到直连仓库。
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -trimpath -o /out/ka ./cmd/ka

# 语料按固定版本克隆：来源路径要能对应到确定的提交，重建索引才可复现。
FROM alpine/git AS corpus
RUN git clone --depth 1 -b v0.54.1 https://github.com/kart-io/goagent /corpus/goagent \
	&& git clone https://github.com/kart-io/goagent-contrib /corpus/goagent-contrib \
	&& git -C /corpus/goagent-contrib checkout --quiet 396d3eca4884 \
	&& rm -rf /corpus/goagent/.git /corpus/goagent-contrib/.git

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /out/ka /usr/local/bin/ka
COPY --from=corpus /corpus /corpus
USER 65534:65534
CMD ["ka", "serve"]
