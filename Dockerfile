FROM golang:1.26-bookworm AS build
WORKDIR /src
# 国内网络环境构建时覆盖：docker build --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/docker-forward ./cmd/docker-forward

FROM scratch
COPY --from=build /out/docker-forward /docker-forward
# 需要挂载 /var/run/docker.sock 并以 --network host 运行
ENTRYPOINT ["/docker-forward"]
CMD ["-rules", "/etc/docker-forward/rules.json"]
