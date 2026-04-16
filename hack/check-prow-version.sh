#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail
set -o xtrace

# Ensures sigs.k8s.io/prow resolves to the same module version in ci-tools,
# release-controller, and ci-chat-bot.
#
# With sibling clones (../release-controller, ../ci-chat-bot), uses go list.
# Otherwise fetches go.mod from GitHub for the peer branch (PULL_BASE_REF or main).

function prow_module_line_from_dir() {
	local dir="$1"
	(
		cd "${dir}"
		go list -mod=mod -m sigs.k8s.io/prow
	)
}

function prow_module_line_from_github() {
	local orgrepo="$1"
	local branch="${2:-main}"
	local url="https://raw.githubusercontent.com/${orgrepo}/${branch}/go.mod"
	local modfile
	modfile="$( mktemp )"
	curl -fsSL "${url}" -o "${modfile}"
	local line
	line="$( grep -E '^[[:space:]]*sigs\.k8s\.io/prow[[:space:]]+v' "${modfile}" | grep -v '^replace' | head -1 )" || true
	rm -f "${modfile}"
	if [[ -z "${line}" ]]; then
		echo "[FATAL] could not find sigs.k8s.io/prow require in ${url}" >&2
		return 1
	fi
	echo "sigs.k8s.io/prow $( echo "${line}" | awk '{print $2}' )"
}

function prow_module_line_peer() {
	local orgrepo="$1"
	local sibling_dir="$2"
	local branch="${PULL_BASE_REF:-main}"

	if [[ -f "${sibling_dir}/go.mod" ]]; then
		prow_module_line_from_dir "${sibling_dir}"
	else
		prow_module_line_from_github "${orgrepo}" "${branch}"
	fi
}

ci_tools_version="$( prow_module_line_from_dir . )"
release_controller_version="$( prow_module_line_peer openshift/release-controller "${PWD}/../release-controller" )"
ci_chat_bot_version="$( prow_module_line_peer openshift/ci-chat-bot "${PWD}/../ci-chat-bot" )"

if [[ "${ci_tools_version}" != "${release_controller_version}" ]] || [[ "${ci_tools_version}" != "${ci_chat_bot_version}" ]]; then
	echo "[FATAL] sigs.k8s.io/prow must match across ci-tools, release-controller, and ci-chat-bot."
	echo "  ci-tools:            ${ci_tools_version}"
	echo "  release-controller:  ${release_controller_version}"
	echo "  ci-chat-bot:         ${ci_chat_bot_version}"
	exit 1
fi
