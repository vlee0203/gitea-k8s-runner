# 构建型执行环境：普通工具链 + stock gitea-runner + kaniko + `docker` 替身。
#
# 用它跑的任务可以照 GitHub 标准写 `docker login` / `docker build` / `docker push`
# （见 container/docker.sh：同名 `docker` 把命令翻译给 kaniko）。
#
# 为什么构建任务要单独一个容器：本平台只能靠 kaniko 构建（baseline 命名空间不许特权，
# docker/podman/buildah 都要挂载），而 kaniko 会把基础镜像解包到**自己容器的根文件系统**上，
# 所以它必须独占一个容器 —— 不能和普通任务挤在通用池镜像里。
# 由 controller 按 job 内容自动路由（container/build.Dockerfile 的镜像配 BUILD_IMAGE）。
FROM docker.gitea.com/runner:nightly AS runner
FROM ghcr.io/osscontainertools/kaniko:v1.28.6-debug AS kaniko

FROM docker.gitea.com/runner-images:ubuntu-latest

# stock gitea-runner（一次性、跑一个 job 后退出）
COPY --from=runner /usr/local/bin/gitea-runner /usr/local/bin/gitea-runner
COPY --from=runner /usr/local/bin/run.sh /usr/local/bin/run.sh

# kaniko：本平台唯一的构建器
COPY --from=kaniko /kaniko/executor /kaniko/executor

# `docker` 替身：把 docker CLI 用法翻译成 kaniko（在 PATH 前面，覆盖同名命令）
COPY container/docker.sh /usr/local/bin/docker
RUN chmod +x /usr/local/bin/docker /kaniko/executor

# HOME 落在 /kaniko（kaniko 构建时不清自己的目录）：这样 runner 的工作目录与
# 构建上下文不会被它自己的清空动作删掉。见 container/docker.sh 顶部说明。
ENV HOME=/kaniko/home
RUN mkdir -p /kaniko/home /kaniko/rt

ENTRYPOINT ["/usr/local/bin/run.sh"]
