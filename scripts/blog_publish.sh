#!/usr/bin/env bash
# Publish a directory as a PRism blog post: metadata first, then every file
# under <dir> with its relative path preserved, then remove stored files that
# are no longer in <dir>, then the publish flag.
#
#   PRISM_BASE_URL=https://prism.example.com \
#     scripts/blog_publish.sh ./site my-post "Title" "One-line dek" [--publish]
#
# Authenticates with the GitHub CLI token of the current user, who must be a
# PRism admin. Needs curl, jq and gh.
set -euo pipefail

usage() {
  echo "usage: $0 <dir> <slug> \"<title>\" \"<dek>\" [--publish]" >&2
  exit 2
}

[ $# -ge 4 ] || usage
dir=${1%/}
slug=$2
title=$3
dek=$4
shift 4
publish=false
for arg in "$@"; do
  case "$arg" in
    --publish) publish=true ;;
    *) usage ;;
  esac
done

[ -d "$dir" ] || { echo "not a directory: $dir" >&2; exit 2; }
[ -f "$dir/index.html" ] || { echo "$dir/index.html is required" >&2; exit 2; }
[[ "$slug" =~ ^[a-z0-9][a-z0-9-]{1,80}$ ]] || { echo "slug must match ^[a-z0-9][a-z0-9-]{1,80}$" >&2; exit 2; }
for tool in curl jq gh; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done

base=${PRISM_BASE_URL:?set PRISM_BASE_URL to the PRism server URL}
base=${base%/}
token=$(gh auth token)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The token travels in a curl config on stdin so it never appears in the
# process list. Prints the HTTP status; the body lands in $tmp/body.
request() {
  local method=$1 path=$2 content_type=$3 data_file=${4:-}
  local args=(-sS -o "$tmp/body" -w '%{http_code}' -X "$method" -H "Content-Type: $content_type" --config -)
  [ -n "$data_file" ] && args+=(--data-binary "@$data_file")
  printf 'header = "Authorization: Bearer %s"\nurl = "%s%s"\n' "$token" "$base" "$path" | curl "${args[@]}"
}

fail() {
  echo "$1 (HTTP $2): $(head -c 300 "$tmp/body")" >&2
  exit 1
}

content_type() {
  case "${1##*.}" in
    html|HTML) echo text/html ;;
    png|PNG) echo image/png ;;
    jpg|jpeg|JPG|JPEG) echo image/jpeg ;;
    gif|GIF) echo image/gif ;;
    webp) echo image/webp ;;
    svg) echo image/svg+xml ;;
    webm) echo video/webm ;;
    mp4) echo video/mp4 ;;
    css) echo text/css ;;
    woff2) echo font/woff2 ;;
    json) echo application/json ;;
    txt|md) echo text/plain ;;
    *) file --mime-type -b "$1" ;;
  esac
}

post_path="/api/blog/posts/$slug"

# Keep an existing post's publish state while its files are replaced.
current_published=false
: > "$tmp/remote_files"
status=$(request GET "$post_path" application/json)
case "$status" in
  200)
    current_published=$(jq -r '.post.published' "$tmp/body")
    jq -r '.files[].path' "$tmp/body" > "$tmp/remote_files"
    ;;
  404) ;;
  *) fail "could not read $slug" "$status" ;;
esac

jq -n --arg title "$title" --arg dek "$dek" --argjson published "$current_published" \
  '{title: $title, dek: $dek, published: $published}' > "$tmp/meta.json"
status=$(request PUT "$post_path" application/json "$tmp/meta.json")
[[ "$status" == 200 || "$status" == 201 ]] || fail "could not save metadata for $slug" "$status"
echo "metadata saved: $slug"

: > "$tmp/local_files"
while IFS= read -r -d '' file; do
  rel=${file#"$dir"/}
  echo "$rel" >> "$tmp/local_files"
  type=$(content_type "$file")
  status=$(request PUT "$post_path/files/$rel" "$type" "$file")
  [ "$status" == 200 ] || fail "upload failed for $rel" "$status"
  echo "uploaded: $rel ($type, $(wc -c < "$file" | tr -d ' ') bytes)"
done < <(find "$dir" -type f -not -path '*/.*' -print0 | sort -z)

while IFS= read -r stale; do
  [ -n "$stale" ] || continue
  status=$(request DELETE "$post_path/files/$stale" application/json)
  [ "$status" == 200 ] || fail "could not remove $stale" "$status"
  echo "removed: $stale"
done < <(grep -vxF -f "$tmp/local_files" "$tmp/remote_files" || true)

if $publish; then
  jq '.published = true' "$tmp/meta.json" > "$tmp/publish.json"
  status=$(request PUT "$post_path" application/json "$tmp/publish.json")
  [ "$status" == 200 ] || fail "could not publish $slug" "$status"
  echo "published: $base/blog/$slug/"
else
  echo "draft ready (admins only): $base/blog/$slug/"
fi
