#!/bin/zsh
set -euo pipefail

app_path="${CURSOR_APP_PATH:-/Applications/Cursor.app}"
state_dir="${CURSOR_VERSION_WATCH_STATE_DIR:-$HOME/Library/Caches/cursor-proto/version-watch}"
mkdir -p "$state_dir"

installed_version=$(defaults read "$app_path/Contents/Info" CFBundleShortVersionString)
case "$(uname -m)" in
  arm64) platform=darwin-arm64 ;;
  x86_64) platform=darwin-x64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

endpoint="https://api2.cursor.sh/updates/api/update/$platform/cursor/$installed_version/stable"
response_file="$state_dir/response.json"
http_status=$(/usr/bin/curl -sS -o "$response_file.tmp" -w '%{http_code}' --max-time 30 "$endpoint")

if [[ "$http_status" == "204" ]]; then
  rm -f "$response_file.tmp"
  printf '%s current=%s\n' "$(date -u +%FT%TZ)" "$installed_version" > "$state_dir/status.log"
  exit 0
fi

if [[ "$http_status" != "200" ]]; then
  rm -f "$response_file.tmp"
  echo "Cursor update check returned HTTP $http_status" >&2
  exit 1
fi

mv "$response_file.tmp" "$response_file"
latest_version=$(/usr/local/bin/jq -r '.name // empty' "$response_file")
download_url=$(/usr/local/bin/jq -r '.url // empty' "$response_file")
if [[ -z "$latest_version" || -z "$download_url" ]]; then
  echo "Cursor update response is missing name or url" >&2
  exit 1
fi

printf '%s installed=%s latest=%s url=%s\n' \
  "$(date -u +%FT%TZ)" "$installed_version" "$latest_version" "$download_url" > "$state_dir/status.log"

last_notified=$(cat "$state_dir/last-notified" 2>/dev/null || true)
if [[ "$last_notified" != "$latest_version" ]]; then
  osascript -e "display notification \"Cursor $latest_version is available; update the client, then refresh cursor-proto's kernel.\" with title \"cursor-proto version watch\"" || true
  print -r -- "$latest_version" > "$state_dir/last-notified"
fi
