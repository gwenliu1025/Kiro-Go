# builder 阶段始终运行在构建机原生平台（amd64），用 Go 交叉编译目标平台二进制
ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o kiro-go .

FROM alpine:latest
RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=builder /app/kiro-go .
COPY --from=builder /app/web ./web
RUN mkdir -p /app/data

EXPOSE 8321
VOLUME /app/data

CMD ["./kiro-go"]
