# gemini-web2api-go — 本地构建镜像（加入 Chromium CDP 浏览器登录支持）
# 运行用户 uid 65532 (nonroot)，与官方镜像一致；数据卷属主已为 65532。
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 65532 nonroot

COPY --chown=65532:65532 gemini-web2api /app/gemini-web2api
RUN chmod 755 /app/gemini-web2api

WORKDIR /app
USER nonroot:nonroot

ENV DB_PATH=/data/gemini.db
ENV TZ=Asia/Shanghai
EXPOSE 8083

ENTRYPOINT ["/app/gemini-web2api"]
CMD ["--db", "/data/gemini.db", "--port", "8083"]
