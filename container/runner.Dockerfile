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
ENTRYPOINT ["/usr/local/bin/run.sh"]
