VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X cmdgate/internal/cli.Version=$(VERSION)

.PHONY: build test test-isolation check dist test-scripts clean

build:
	go build -ldflags '$(LDFLAGS)' -o cmdgate ./cmd/cmdgate

# 单元测试和以当前用户运行的端到端测试
test:
	go test ./...

# 在一次性的 Linux 容器里验证独立用户下的权限隔离
test-isolation:
	./scripts/test-isolation.sh

check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

# 发布归档和校验和，版本必须为稳定 tag，例如 make dist VERSION=v0.1.0
dist:
	./scripts/build-release.sh '$(VERSION)'

# 在临时目录中验证发布与安装流程，不访问 GitHub 或安装系统服务
test-scripts:
	./scripts/test-release.sh

clean:
	rm -rf cmdgate dist
