set export
set default-script
set script-interpreter := ["bash", "-euo", "pipefail"]

# Build the daemon. This is the default recipe.
build:
    echo "Building omp-telegram..."
    go build -o omp-telegram ./cmd/omp-telegram

# Format with the module's Go version, matching CI.
fmt:
    version=$(go list -m -f '{{ "{{.GoVersion}}" }}')
    GOTOOLCHAIN="go${version}" go fmt ./...

# Install only the binary; configuration and data remain user-managed.
install bin_dir=(env_var("HOME") / "tool/omp-telegram"): build
    echo "Installing omp-telegram to {{bin_dir}}..."
    install -d {{quote(bin_dir)}}
    install -m 755 omp-telegram {{quote(bin_dir / "omp-telegram")}}
    echo "Installed omp-telegram to {{bin_dir / "omp-telegram"}}."

# Build/install run synchronously; the supervisor restart is detached and delayed.
# Run `just deploy` and move on after it returns; do not check its exit status or the restart.
deploy: install
    setsid bash -c 'sleep 5; supervisord ctl restart omp-telegram' >/dev/null 2>&1 < /dev/null &
    echo "Restart scheduled in 5s."

# Run unit tests without touching the supervised daemon.
[positional-arguments]
test *args:
    umask 0077
    go test ./... "$@"

# Run real OMP compatibility tests with an explicitly supplied binary, without installing it.
smoke-omp binary:
    binary={{quote(binary)}}
    if [[ -z "$binary" || ! -f "$binary" || ! -x "$binary" ]]; then
        echo "Expected an explicit path to an executable OMP binary" >&2
        exit 1
    fi
    binary=$(realpath -- "$binary")
    OMP_TELEGRAM_TEST_OMP="$binary" {{quote(just_executable())}} test -run '^TestRealOMP(Bridge|Migration)Compatibility$' -count=1 -v

# Run automated tests, race checks, and vet without starting Telegram polling.
check:
    umask 0077
    go test ./...
    go test -race ./...
    go vet ./...
