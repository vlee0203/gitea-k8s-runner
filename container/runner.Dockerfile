# One-shot executor image: a full toolchain (so `uses: actions/checkout@v4` and
# ordinary `run:` steps work) plus the stock gitea-runner, which registers
# against the controller's facade and runs exactly one job in host mode.
#
# TODO(verify): confirm the runner binary/entrypoint paths in the pinned
# docker.gitea.com/runner image before relying on the copy below; keep the version
# pinned (not :nightly) once validated.
FROM docker.gitea.com/runner:nightly AS runner

FROM docker.gitea.com/runner-images:ubuntu-latest
COPY --from=runner /usr/local/bin/gitea-runner /usr/local/bin/gitea-runner
COPY --from=runner /usr/local/bin/run.sh /usr/local/bin/run.sh

# `docker` 替身：把标准 docker 命令转给同一个 Pod 里的 kaniko 容器（见 container/docker.sh）。
# 它自己不构建 —— 本平台没有任何 docker daemon，kaniko 又必须作为它自己的镜像运行。
COPY container/docker.sh /usr/local/bin/docker
RUN chmod +x /usr/local/bin/docker
ENTRYPOINT ["/usr/local/bin/run.sh"]
