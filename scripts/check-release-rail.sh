#!/usr/bin/env bash
# Release-rail guardrail (AGENTS.md "Release guardrail: two shipping rails").
#
# Component binaries ship via the pacman package rail; user units and session
# scripts (users/**) ship ONLY via the full-image rsync rail built in the ffos
# repo. A release whose diff touches BOTH rails MUST go out as a full-image
# version bump: a package-only rollout would run new binaries under old
# units/scripts, leaving the change silently inert or broken on fielded
# devices.
#
# This repo's CI cannot see the ffos repo (the image build is a manual
# workflow_dispatch there: build-image-to-cf.yml, inputs `version` and
# `ffos_user_ref`), so this check enforces the strongest thing visible from
# here: a cross-rail release PR must carry a full-image declaration in
# RELEASES.md. That gives durable, reviewable evidence that the release is
# being cut on the full-image rail and hard-stops a silent package-only
# release; actually dispatching the ffos image build remains the release
# operator's step, with the exact parameters recorded in the ledger entry.
#
# Usage: check-release-rail.sh <base-ref>   (e.g. origin/staging)
set -euo pipefail

base="${1:?usage: check-release-rail.sh <base-ref>}"

fail() {
  echo "check-release-rail: $*" >&2
  exit 1
}

changed="$(git diff --name-only "$base"...HEAD)"

package_rail="$(grep -E '^components/' <<<"$changed" || true)"
image_rail="$(grep -E '^users/' <<<"$changed" || true)"

if [ -z "$package_rail" ] || [ -z "$image_rail" ]; then
  echo "check-release-rail: OK (single-rail diff vs $base; no full-image declaration required)"
  exit 0
fi

echo "check-release-rail: cross-rail diff vs $base detected"
echo "--- package rail (components/**):"
head -20 <<<"$package_rail"
echo "--- image rail (users/**):"
head -20 <<<"$image_rail"

if ! grep -qx 'RELEASES.md' <<<"$changed"; then
  fail "diff touches BOTH shipping rails but does not update RELEASES.md.
This release must ship as a full-image version bump (AGENTS.md 'Release
guardrail: two shipping rails'): a package-only rollout would run the new
binaries under the old units/scripts. Add a RELEASES.md entry declaring the
full-image release (version, ffos build-image-to-cf.yml dispatch parameters)."
fi

# The entry added by THIS release must itself declare full-image — an
# unrelated RELEASES.md edit must not satisfy the guardrail.
added_lines="$(git diff "$base"...HEAD -- RELEASES.md | grep '^+' || true)"
if ! grep -qi 'full-image' <<<"$added_lines"; then
  fail "RELEASES.md changed, but the added lines carry no 'full-image' declaration for this release."
fi

echo "check-release-rail: OK (cross-rail diff carries a full-image declaration in RELEASES.md)"
