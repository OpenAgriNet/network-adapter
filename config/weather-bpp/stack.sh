#!/usr/bin/env bash
# Start / stop / inspect the weather:forecast test stack.
#
#   ./config/weather-bpp/stack.sh start [signed|local]
#   ./config/weather-bpp/stack.sh stop
#   ./config/weather-bpp/stack.sh restart [signed|local]
#   ./config/weather-bpp/stack.sh status
#   ./config/weather-bpp/stack.sh logs [adapter|backend|registry|sink]
#   ./config/weather-bpp/stack.sh test [signed|local]
#
# Processes are tracked by PORT, not by name. `go run` forks a compiler parent
# and a binary child with different names, so `pkill -f weather-backend` kills
# one and leaves the other holding the socket -- which then fails the next
# start with "address already in use".
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOGS=/tmp/weather-stack
FIXTURES=/tmp

ADAPTER_PORT=8082
BACKEND_PORT=9000
REGISTRY_PORT=8090
SINK_PORT=7000
REDIS_PORT=6379

pid_on () { lsof -nP -iTCP:"$1" -sTCP:LISTEN -t 2>/dev/null | head -1; }

# Start each service in its OWN session, so it outlives this script, the shell
# that ran it, and the terminal window. `nohup` alone only blocks SIGHUP -- the
# process stays in the caller's process tree and dies with it. macOS ships no
# setsid(1); perl's POSIX::setsid is the portable stand-in. Fall back to nohup
# where perl is missing, and say so, because the difference only shows up later
# as "why did my stack vanish".
if perl -e 'use POSIX; exit 0' 2>/dev/null; then
    DETACH="perl -MPOSIX -e POSIX::setsid();exec(@ARGV); --"
else
    DETACH="nohup"
    echo "  note: perl not found; services will not survive closing this terminal" >&2
fi

wait_for () {  # wait_for <port> <up|down> [timeout-seconds]
    local port=$1 want=$2 limit=${3:-25} n=0
    while [ $n -lt $((limit * 10)) ]; do
        local p; p=$(pid_on "$port")
        [ "$want" = up ]   && [ -n "$p" ] && return 0
        [ "$want" = down ] && [ -z "$p" ] && return 0
        perl -e 'select undef,undef,undef,0.1' 2>/dev/null || sleep 1
        n=$((n + 1))
    done
    return 1
}

kill_port () {  # kill_port <port> <label>
    local pid; pid=$(pid_on "$1")
    if [ -z "$pid" ]; then printf '  %-10s already stopped\n' "$2"; return; fi

    # `go run` leaves two processes: a compiler parent and the binary child that
    # actually holds the socket. Kill both, or the survivor keeps the port and
    # the next start fails with "address already in use".
    #
    # Do NOT kill by process group. Everything this script launches shares the
    # script's group, so `kill -- -$pgid` would take down all four services when
    # asked to stop one.
    local ppid; ppid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')
    local parent_cmd=""
    [ -n "$ppid" ] && [ "$ppid" != 1 ] && parent_cmd=$(ps -o command= -p "$ppid" 2>/dev/null)

    kill -TERM "$pid" 2>/dev/null
    case "$parent_cmd" in *"go run"*) kill -TERM "$ppid" 2>/dev/null ;; esac

    if wait_for "$1" down 5; then printf '  %-10s stopped\n' "$2"; return; fi
    kill -KILL "$pid" 2>/dev/null
    case "$parent_cmd" in *"go run"*) kill -KILL "$ppid" 2>/dev/null ;; esac
    wait_for "$1" down 5 && printf '  %-10s killed\n' "$2" \
                         || printf '  %-10s WOULD NOT DIE (pid %s)\n' "$2" "$pid"
}

# A key that is empty, or still wearing angle brackets, is a placeholder someone
# pasted from the docs and never filled in. Treat it as absent -- otherwise it
# reaches OpenWeatherMap, 401s, and surfaces fifteen seconds later as a
# mystifying "no callback arrived" in an unrelated test.
usable_key () { case "${1:-}" in ""|*"<"*|*">"*) return 1 ;; *) return 0 ;; esac }

api_key () {
    local k
    k="${OWM_API_KEY:-}";                                       usable_key "$k" && { echo "$k"; return; }
    k=$(grep -ho 'OWM_API_KEY=.*' "$REPO/.env" 2>/dev/null | head -1 | cut -d= -f2-)
    usable_key "$k" && { echo "$k"; return; }
    k=$(grep -ho 'API_KEY=.*' "$REPO/open-weather.md" 2>/dev/null | head -1 | cut -d= -f2-)
    usable_key "$k" && { echo "$k"; return; }
    return 1
}

# Fail at start, where the message can name the cause, rather than inside a
# test run where it cannot.
check_key () {
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        "https://api.openweathermap.org/data/2.5/forecast?lat=12.97&lon=77.59&appid=$1")
    case "$code" in
        200) printf '  %-10s key accepted by OpenWeatherMap\n' upstream ;;
        401) printf '  %-10s KEY REJECTED (401). Folders 01-03 and 05 still pass;\n' upstream
             printf '  %-10s folder 04 needs a live forecast and will fail.\n' "" ;;
        000) printf '  %-10s unreachable (offline?). Folder 04 will fail.\n' upstream ;;
        *)   printf '  %-10s unexpected HTTP %s\n' upstream "$code" ;;
    esac
}

start_fixture () {  # start_fixture <dir> <port> <label> [env-assignment]
    local dir=$1 port=$2 label=$3 envassign=${4:-}
    if [ -n "$(pid_on "$port")" ]; then printf '  %-10s already up on :%s\n' "$label" "$port"; return; fi
    if [ ! -d "$dir" ]; then printf '  %-10s MISSING at %s\n' "$label" "$dir"; return 1; fi
    ( cd "$dir" && env $envassign $DETACH go run . <"/dev/null" >"$LOGS/$label.log" 2>&1 & )
    if wait_for "$port" up 60; then printf '  %-10s up on :%s\n' "$label" "$port"
    else printf '  %-10s FAILED -- %s\n' "$label" "$LOGS/$label.log"; tail -3 "$LOGS/$label.log"; return 1; fi
}

cmd_start () {
    local mode=${1:-signed} cfg
    case "$mode" in
        signed) cfg=adaptor-signed.yaml ;;
        local)  cfg=adaptor.yaml ;;
        *) echo "mode must be 'signed' or 'local'"; exit 2 ;;
    esac
    mkdir -p "$LOGS"

    if [ -z "$(pid_on $REDIS_PORT)" ]; then
        echo "  redis      NOT RUNNING on :$REDIS_PORT -- the cache plugin needs it."
        echo "             docker run -d -p 6379:6379 redis:alpine"
        exit 1
    fi
    printf '  %-10s up on :%s\n' redis "$REDIS_PORT"

    local key
    if ! key=$(api_key); then
        echo "  backend    no usable OpenWeatherMap key."
        echo "             Looked at: \$OWM_API_KEY, $REPO/.env, $REPO/open-weather.md"
        echo "             A value like <new-key> is a placeholder and is ignored."
        exit 1
    fi
    check_key "$key"

    start_fixture "$FIXTURES/mock-registry"   $REGISTRY_PORT registry || exit 1
    start_fixture "$FIXTURES/bap-sink"        $SINK_PORT     sink     || exit 1
    start_fixture "$FIXTURES/weather-backend" $BACKEND_PORT  backend "OWM_API_KEY=$key" || exit 1

    if [ -n "$(pid_on $ADAPTER_PORT)" ]; then
        printf '  %-10s already up on :%s (stop first to switch config)\n' adapter "$ADAPTER_PORT"
    else
        [ -x "$REPO/server" ] || ( cd "$REPO" && go build -o server ./cmd/adapter ) || exit 1
        ( cd "$REPO" && $DETACH ./server --config "./config/weather-bpp/$cfg" \
              <"/dev/null" >"$LOGS/adapter.log" 2>&1 & )
        if wait_for $ADAPTER_PORT up 30; then
            printf '  %-10s up on :%s  (%s)\n' adapter "$ADAPTER_PORT" "$cfg"
        else
            printf '  %-10s FAILED\n' adapter
            grep '"level":"fatal"' "$LOGS/adapter.log" | tail -1
            exit 1
        fi
    fi
    echo
    echo "  test it:  $0 test $mode"
}

cmd_stop () {
    kill_port $ADAPTER_PORT  adapter
    kill_port $BACKEND_PORT  backend
    kill_port $SINK_PORT     sink
    kill_port $REGISTRY_PORT registry
    echo "  redis      left running (shared, and not ours to stop)"
}

cmd_status () {
    printf '  %-10s %-6s %-8s %s\n' SERVICE PORT PID STATE
    for row in "adapter $ADAPTER_PORT" "backend $BACKEND_PORT" \
               "registry $REGISTRY_PORT" "sink $SINK_PORT" "redis $REDIS_PORT"; do
        set -- $row
        local pid; pid=$(pid_on "$2")
        printf '  %-10s %-6s %-8s %s\n' "$1" "$2" "${pid:--}" \
               "$([ -n "$pid" ] && echo up || echo DOWN)"
    done
    local pid; pid=$(pid_on "$ADAPTER_PORT")
    if [ -n "$pid" ]; then
        echo
        echo "  adapter config: $(ps -o command= -p "$pid" | sed 's/.*--config //')"
    fi
}

cmd_test () {
    local mode=${1:-signed} env_file extra=()
    if [ "$mode" = local ]; then
        env_file="$REPO/postman/weather-local-unsigned.postman_environment.json"
        # Folder 01 tests validateSign, which adaptor.yaml does not run.
        extra=(--folder "02 · Schema rejections"
               --folder "03 · Lifecycle (signed, real upstream)"
               --folder "04 · Forecast delivery"
               --folder "05 · Transform in isolation")
    else
        env_file="$REPO/postman/weather-signed.postman_environment.json"
    fi
    # ${extra[@]+...} guards the empty-array case: bash 3.2, which is what macOS
    # ships, treats "${extra[@]}" as an unbound variable under `set -u`.
    npx --yes newman run "$REPO/postman/weather-adapter.postman_collection.json" \
        -e "$env_file" ${extra[@]+"${extra[@]}"} --reporters cli
}

case "${1:-status}" in
    start)   shift; cmd_start "${1:-signed}" ;;
    stop)    cmd_stop ;;
    restart) shift; cmd_stop; echo; cmd_start "${1:-signed}" ;;
    status)  cmd_status ;;
    test)    shift; cmd_test "${1:-signed}" ;;
    logs)    tail -f "$LOGS/${2:-adapter}.log" ;;
    *) sed -n '2,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' ;;
esac
