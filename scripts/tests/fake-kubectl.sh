#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "--context" ]]; then
  requested_context="${2:-}"
  shift 2
  if [[ "$requested_context" != "${FAKE_CONTEXT:-fixture-context}" ]]; then
    printf 'unexpected --context %s\n' "$requested_context" >&2
    exit 1
  fi
fi

command_name="${1:-}"
shift || true

case "$command_name" in
  config)
    [[ "${1:-}" == "current-context" ]] || exit 1
    printf '%s\n' "${FAKE_CONTEXT:-fixture-context}"
    ;;
  get)
    resource="${1:-}"
    name="${2:-}"
    case "${resource}/${name}" in
      certificate.cert-manager.io/dns-tls)
        file="${FIXTURE_DIR:?}/certificate.json"
        ;;
      secret/dns-tls)
        file="${FIXTURE_DIR:?}/secret.json"
        ;;
      gateway.gateway.networking.k8s.io/paprika)
        file="${FIXTURE_DIR:?}/gateway.json"
        ;;
      *)
        printf 'unexpected get target %s/%s\n' "$resource" "$name" >&2
        exit 1
        ;;
    esac
    [[ -f "$file" ]] || exit 1
    jq . "$file"
    ;;
  patch)
    resource="${1:-}"
    name="${2:-}"
    shift 2
    [[ "$resource" == "gateway.gateway.networking.k8s.io" && "$name" == "paprika" ]] || exit 1
    patch=""
    while (($# > 0)); do
      case "$1" in
        --patch)
          patch="${2:-}"
          shift 2
          ;;
        --type=json|-o)
          if [[ "$1" == -o ]]; then
            shift 2
          else
            shift
          fi
          ;;
        -n)
          shift 2
          ;;
        *)
          printf 'unexpected patch argument %s\n' "$1" >&2
          exit 1
          ;;
      esac
    done
    [[ -n "$patch" ]] || exit 1
    printf '%s\n' "$patch" >>"${PATCH_LOG:?}"

    add_count="$(jq '[.[] | select(.op == "add")] | length' <<<"$patch")"
    [[ "$add_count" == 1 ]] || exit 1
    add_path="$(jq -r '.[] | select(.op == "add") | .path' <<<"$patch")"
    if [[ ! "$add_path" =~ ^/spec/listeners/([0-9]+)/tls/certificateRefs/-$ ]]; then
      exit 1
    fi
    listener_index="${BASH_REMATCH[1]}"
    target="$(jq -c '.[] | select(.op == "add") | .value' <<<"$patch")"
    jq \
      --argjson index "$listener_index" \
      --argjson target "$target" '
        .metadata.resourceVersion = "12346"
        | .spec.listeners[$index].tls.certificateRefs += [$target]
      ' "${FIXTURE_DIR:?}/gateway.json"
    ;;
  *)
    printf 'unexpected fake kubectl command %s\n' "$command_name" >&2
    exit 1
    ;;
esac
